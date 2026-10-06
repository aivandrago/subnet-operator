/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package crdversions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultPreviousCAGrace is how long a CA stays in the CRDs' conversion after this replica
	// last read it from the webhook certificate. A replica serves the previous certificate
	// until the kubelet refreshes its mounted Secret, a minute or two; an hour is the time the
	// chart-signed certificate keeps its own previous CA in ca.crt.
	DefaultPreviousCAGrace = time.Hour
	// MaxPreviousCAs bounds how many certificates that are no longer in ca.crt are kept, the
	// most recently read first: certificates renewed again and again within the grace period
	// must not grow the CRDs without limit.
	MaxPreviousCAs = 4
	// caKey is where a kubernetes.io/tls Secret carries the CA of its certificate.
	caKey = "ca.crt"
)

// Authorities are the CA certificates this replica has itself read from the webhook
// certificate, and so the only ones it puts into the CRDs' conversion: those ca.crt holds now,
// and those it held until less than Grace ago.
//
// A renewed certificate under a new CA reaches the replicas one by one, when the kubelet
// refreshes each one's mounted Secret. Until the last of them has it, the API server meets both
// certificates and has to trust both CAs, so the previous one is kept next to the new one. The
// new one, in turn, has to be in the CRDs before the first replica serves under it, which the
// mounted file cannot tell: the kubelet may refresh another replica first. So ca.crt is read
// from two places: the mounted file, and the Secret it is mounted from, watched through the
// API, which changes before any mount does.
//
// What was read is remembered in memory only, by every replica from its start, leader or not:
// a standby that takes the lease in the middle of a renewal has read both CAs itself. A replica
// started after the renewal has not, and does not keep the previous CA: nothing outside its
// own readings of ca.crt, the content of the CRDs included, can put a certificate into the
// bundle it writes.
type Authorities struct {
	// File is ca.crt next to the mounted serving certificate.
	File string
	// Secret is the Secret File is mounted from. Empty reads the file only.
	Secret types.NamespacedName
	// Watcher watches Secret. Nil reads the file only.
	Watcher client.WithWatch
	// Grace is how long a certificate is kept after it was last read. Zero means
	// DefaultPreviousCAGrace.
	Grace time.Duration
	// Interval is how often Start reads File, and how long it waits before watching Secret
	// again after a failure. Zero means a minute.
	Interval time.Duration

	// now is the clock, for tests.
	now func() time.Time

	mu sync.Mutex
	// known is every certificate read and not yet forgotten.
	known map[[sha256.Size]byte]*authority
	// fromFile and fromSecret are the certificates of the latest reading of each place.
	fromFile, fromSecret [][sha256.Size]byte
	// readings counts the readings that found a certificate not known before.
	readings int
	// changed has room for one notification: a second one before the first is taken says
	// nothing new.
	changed chan struct{}
}

// authority is one CA certificate and when this replica read it.
type authority struct {
	cert *x509.Certificate
	// reading and index order the certificates by when they were first read: a later reading
	// first, and within one reading the order of the file.
	reading, index int
	// lastRead is the last moment the certificate was in ca.crt, as far as this replica saw.
	lastRead time.Time
}

// NeedLeaderElection makes every replica remember what it read: the standby that takes over in
// the middle of a renewal needs to have read the previous CA itself.
func (a *Authorities) NeedLeaderElection() bool {
	return false
}

// Changed is signalled when the certificates ca.crt holds have changed, in the file or in the
// Secret.
func (a *Authorities) Changed() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.notifications()
}

// notifications is the channel behind Changed. The caller holds mu.
func (a *Authorities) notifications() chan struct{} {
	if a.changed == nil {
		a.changed = make(chan struct{}, 1)
	}
	return a.changed
}

// Start reads the file every Interval and watches the Secret until the context ends.
func (a *Authorities) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("crd-versions")
	interval := a.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	var watching sync.WaitGroup
	if a.Watcher != nil && a.Secret.Name != "" {
		watching.Go(func() { a.watchSecret(ctx, interval) })
	}
	defer watching.Wait()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// An unreadable file is the business of whoever writes the CRDs, which reads it again
		// and says so; here it only means that nothing new was read.
		if err := a.readFile(); err != nil {
			log.V(1).Info("Could not read the CA of the webhook certificate", "file", a.File, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// watchSecret keeps the latest ca.crt of the Secret, watching again whenever the watch ends.
func (a *Authorities) watchSecret(ctx context.Context, retry time.Duration) {
	log := logf.FromContext(ctx).WithName("crd-versions").WithValues("secret", a.Secret.String())
	for ctx.Err() == nil {
		err := a.watchSecretOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// The API server ends every watch after a while; what was read still stands.
			continue
		}
		// Without the watch nothing says whether the Secret still holds what was read from it.
		// Its certificates are kept for the grace period like any other that was read before,
		// and the mounted file is still read.
		log.Error(err, "Could not watch the Secret of the webhook certificate; a renewed CA is only read from the "+
			"mounted file until the watch works again", "retryIn", retry)
		a.observe(&a.fromSecret, nil)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// watchSecretOnce watches the Secret until the API server ends the watch (nil) or it fails. A
// watch that names no resource version begins with the Secret as it is.
func (a *Authorities) watchSecretOnce(ctx context.Context) error {
	// As client-go's reflectors do: ended by the API server after five to ten minutes, so that
	// a connection that died silently is replaced.
	timeout := int64(300 + rand.IntN(300)) // #nosec G404 -- spreads reconnects; not a secret.
	w, err := a.Watcher.Watch(ctx, &corev1.SecretList{}, &client.ListOptions{
		Namespace:     a.Secret.Namespace,
		FieldSelector: fields.OneTermEqualSelector("metadata.name", a.Secret.Name),
		Raw:           &metav1.ListOptions{TimeoutSeconds: &timeout},
	})
	if err != nil {
		return err
	}
	defer w.Stop()
	for event := range w.ResultChan() {
		switch event.Type {
		case watch.Added, watch.Modified:
			secret, ok := event.Object.(*corev1.Secret)
			if !ok {
				return fmt.Errorf("the watch delivered a %T, not a Secret", event.Object)
			}
			// A Secret cert-manager is still filling, or one without ca.crt, holds no CA: the
			// file decides alone, as it does without a Secret to watch.
			certs, err := parseCertificates(secret.Data[caKey])
			if err != nil {
				logf.FromContext(ctx).WithName("crd-versions").V(1).Info("Found no CA in the Secret of the webhook certificate",
					"secret", a.Secret.String(), "error", err.Error())
			}
			a.observe(&a.fromSecret, certs)
		case watch.Deleted:
			a.observe(&a.fromSecret, nil)
		case watch.Error:
			return apierrors.FromObject(event.Object)
		}
	}
	return nil
}

// readFile reads ca.crt from the mounted file. An error means the file holds no CA now.
func (a *Authorities) readFile() error {
	raw, err := os.ReadFile(a.File)
	var certs []*x509.Certificate
	if err == nil {
		certs, err = parseCertificates(raw)
	}
	a.observe(&a.fromFile, certs)
	return err
}

// observe records one reading of ca.crt, from the file or from the Secret.
func (a *Authorities) observe(place *[][sha256.Size]byte, certs []*x509.Certificate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	before := a.current()
	// What the reading replaces was in ca.crt until this moment.
	a.touch(before, now)

	if a.known == nil {
		a.known = map[[sha256.Size]byte]*authority{}
	}
	read := make([][sha256.Size]byte, 0, len(certs))
	added := false
	for i, cert := range certs {
		id := sha256.Sum256(cert.Raw)
		if slices.Contains(read, id) {
			continue
		}
		read = append(read, id)
		if _, ok := a.known[id]; !ok {
			if !added {
				a.readings++
				added = true
			}
			a.known[id] = &authority{cert: cert, reading: a.readings, index: i}
		}
		a.known[id].lastRead = now
	}
	*place = read

	after := a.current()
	a.previous(after, now)
	slices.SortFunc(before, compareIDs)
	slices.SortFunc(after, compareIDs)
	if !slices.Equal(before, after) {
		select {
		case a.notifications() <- struct{}{}:
		default:
		}
	}
}

// Bundle reads the file again and returns the certificates for the CRDs' conversion, in PEM:
// those ca.crt holds now, in the file or in the Secret, and those it held until less than Grace
// ago, unless they have expired, at most MaxPreviousCAs of them. An error means ca.crt holds no
// certificate anywhere: there is then nothing the API server could trust the webhook with.
func (a *Authorities) Bundle() ([]byte, error) {
	fileErr := a.readFile()

	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	current := a.current()
	if len(current) == 0 {
		if fileErr == nil {
			fileErr = errors.New("no certificate")
		}
		return nil, fileErr
	}
	a.touch(current, now)

	previous := a.previous(current, now)

	// The same certificates give the same bytes whichever place they were read from, so the
	// CRDs are not written again when the mounted file catches up with the Secret.
	all := append(slices.Clone(current), previous...)
	slices.SortFunc(all, a.firstRead)
	var bundle bytes.Buffer
	for _, id := range all {
		if err := pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: a.known[id].cert.Raw}); err != nil {
			return nil, err
		}
	}
	return bundle.Bytes(), nil
}

// previous is the certificates that are kept although ca.crt no longer holds them, the one
// read last first. It forgets the others: a standby, which never writes a bundle, would
// otherwise remember every certificate it ever read. The caller holds mu.
func (a *Authorities) previous(current [][sha256.Size]byte, now time.Time) [][sha256.Size]byte {
	grace := a.Grace
	if grace <= 0 {
		grace = DefaultPreviousCAGrace
	}
	var previous [][sha256.Size]byte
	for id, known := range a.known {
		switch {
		case slices.Contains(current, id):
		case now.Sub(known.lastRead) >= grace, now.After(known.cert.NotAfter):
			// Expired, it verifies nothing; after the grace period, every replica has long
			// been given the certificate that replaced it.
			delete(a.known, id)
		default:
			previous = append(previous, id)
		}
	}
	slices.SortFunc(previous, func(x, y [sha256.Size]byte) int {
		if c := a.known[y].lastRead.Compare(a.known[x].lastRead); c != 0 {
			return c
		}
		return a.firstRead(x, y)
	})
	for len(previous) > MaxPreviousCAs {
		delete(a.known, previous[len(previous)-1])
		previous = previous[:len(previous)-1]
	}
	return previous
}

// current is what ca.crt holds at the latest reading of the file and of the Secret. The two
// differ while the kubelet has not refreshed the mount yet. The caller holds mu.
func (a *Authorities) current() [][sha256.Size]byte {
	current := slices.Clone(a.fromFile)
	for _, id := range a.fromSecret {
		if !slices.Contains(current, id) {
			current = append(current, id)
		}
	}
	return current
}

// touch marks certificates as in ca.crt at this moment. The caller holds mu.
func (a *Authorities) touch(ids [][sha256.Size]byte, now time.Time) {
	for _, id := range ids {
		a.known[id].lastRead = now
	}
}

// firstRead orders two known certificates: the one read first comes last, and certificates of
// one reading keep the order they had there. The caller holds mu.
func (a *Authorities) firstRead(x, y [sha256.Size]byte) int {
	if c := a.known[y].reading - a.known[x].reading; c != 0 {
		return c
	}
	if c := a.known[x].index - a.known[y].index; c != 0 {
		return c
	}
	return compareIDs(x, y)
}

func (a *Authorities) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func compareIDs(x, y [sha256.Size]byte) int {
	return bytes.Compare(x[:], y[:])
}

// parseCertificates returns the certificates of a PEM bundle. A bundle without one, or with one
// that does not parse, is an error: the API server would refuse it as a caBundle.
func parseCertificates(bundle []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", len(certs)+1, err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificate in it")
	}
	return certs, nil
}
