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
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bundleOf is the CA bundle of a CRD's conversion webhook; nothing while it has none.
func bundleOf(k Kind) []byte {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil {
		return nil
	}
	return crd.Spec.Conversion.Webhook.ClientConfig.CABundle
}

// cas is a bundle of the named test CAs, in that order.
func cas(names ...string) []byte {
	GinkgoHelper()
	bundle := &bytes.Buffer{}
	for _, name := range names {
		bundle.Write(testCA(name))
	}
	return bundle.Bytes()
}

// A renewal of the webhook certificate under a new CA, as the leader sees it in its mounted
// ca.crt: the CRDs get the new CA and keep the previous one for as long as a replica may still
// serve under it, and they never keep a certificate the operator did not read from ca.crt.
var _ = Describe("The CAs of the conversion webhook across a renewal", Ordered, func() {
	var (
		caFile  string
		now     time.Time
		service = types.NamespacedName{Namespace: "subnets", Name: "subnet-operator-webhook"}
		u       *Upgrader
	)
	write := func(names ...string) {
		GinkgoHelper()
		Expect(os.WriteFile(caFile, cas(names...), 0o600)).To(Succeed())
	}
	expectBundles := func(names ...string) {
		GinkgoHelper()
		for _, k := range Kinds {
			Expect(string(bundleOf(k))).To(Equal(string(cas(names...))), k.CRDName())
		}
	}

	BeforeAll(func() {
		installCRDs0_9()
		upgradeCRDs()
		caFile = filepath.Join(GinkgoT().TempDir(), "ca.crt")
		now = time.Now()
		u = &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service,
			Authorities: &Authorities{File: caFile, now: func() time.Time { return now }}}
	})
	AfterAll(removeCRDs)

	It("writes the CA of ca.crt, and nothing else, on a first issuance", func() {
		write("renewal-1")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-1")
	})

	It("keeps the previous CA next to the new one when ca.crt changes, the new one first", func() {
		now = now.Add(10 * time.Minute)
		write("renewal-2")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2", "renewal-1")
	})

	It("removes a CA somebody else put into a CRD, and keeps the previous CA it read itself", func() {
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
			patched := crd.Spec.Conversion.DeepCopy()
			patched.Webhook.ClientConfig.CABundle = cas("renewal-2", "renewal-1", "foreign")
			Expect(setConversionOf(k, patched)).To(Succeed())
		}
		expectBundles("renewal-2", "renewal-1", "foreign")

		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2", "renewal-1")
	})

	It("does not take a CA for its own because the CRDs hold it", func() {
		// A replica that starts now has read renewal-2 only. The CRDs' bundle is not where it
		// learns what came before: it replaces the foreign CA and the previous one alike.
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
			patched := crd.Spec.Conversion.DeepCopy()
			patched.Webhook.ClientConfig.CABundle = cas("foreign", "renewal-2", "renewal-1")
			Expect(setConversionOf(k, patched)).To(Succeed())
		}
		started := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service, CAFile: caFile}
		Expect(started.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2")

		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2", "renewal-1")
	})

	It("drops the previous CA once the grace period since it left ca.crt has passed", func() {
		// It left ca.crt ten minutes into the spec's time, when renewal-2 was read.
		now = now.Add(DefaultPreviousCAGrace - time.Second)
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2", "renewal-1")

		now = now.Add(time.Second)
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2")
	})

	It("does not bring a dropped CA back, and takes it as new when ca.crt holds it again", func() {
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-2")

		write("renewal-1")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-1", "renewal-2")
	})

	It("drops a previous CA that has expired before the grace period is over, "+
		"and writes an expired one that ca.crt holds", func() {
		u.Authorities.Grace = 100 * time.Hour
		DeferCleanup(func() { u.Authorities.Grace = 0 })
		testCAFor("short-lived", now.Add(time.Hour))
		write("short-lived")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("short-lived", "renewal-1", "renewal-2")
		write("renewal-3")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-3", "short-lived", "renewal-1", "renewal-2")

		now = now.Add(time.Hour + time.Second)
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("renewal-3", "renewal-1", "renewal-2")

		// What ca.crt holds is written whatever its dates: an expired CA there is for whoever
		// issues the certificate to repair, and the API server names it in every refusal.
		write("short-lived")
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("short-lived", "renewal-3", "renewal-1", "renewal-2")
	})

	It("keeps no more previous CAs than MaxPreviousCAs, the latest ones", func() {
		var want []string
		for i := range MaxPreviousCAs + 3 {
			name := fmt.Sprintf("many-%d", i)
			want = append([]string{name}, want...)
			now = now.Add(time.Second)
			write(name)
			Expect(u.ConfigureConversion(ctx)).To(Succeed())
		}
		expectBundles(want[:1+MaxPreviousCAs]...)
	})

	It("keeps both CAs of a ca.crt that carries two, as the chart-signed certificate's does", func() {
		fresh := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service, CAFile: caFile}
		write("chart-new", "chart-previous")
		Expect(fresh.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("chart-new", "chart-previous")

		// The first upgrade an hour later drops the previous CA from ca.crt; the operator keeps
		// it for its own grace period, and no longer.
		write("chart-new")
		Expect(fresh.ConfigureConversion(ctx)).To(Succeed())
		expectBundles("chart-new", "chart-previous")
	})
})

// The same renewal with two replicas, each with its own mounted ca.crt, and the Secret both are
// mounted from. cert-manager writes the Secret, the kubelet refreshes one mount and later the
// other, and the lease changes hands in between.
var _ = Describe("The CAs of the conversion webhook with two replicas", Ordered, func() {
	var (
		secret  = types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "subnet-operator-webhook-cert"}
		service = types.NamespacedName{Namespace: "subnets", Name: "subnet-operator-webhook"}
		watcher client.WithWatch
		stop    func()
		// The leader, and the standby that takes over.
		first, second *replica
	)
	writeSecret := func(names ...string) {
		GinkgoHelper()
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secret.Namespace, Name: secret.Name}}
		if err := k8sClient.Get(ctx, secret, s); err != nil {
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			s.Data = map[string][]byte{caKey: cas(names...)}
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			return
		}
		s.Data = map[string][]byte{caKey: cas(names...)}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}
	// eventuallyBundles is for what a running replica writes on its own, when the Secret
	// changes: its interval is an hour, so a tick is not what wrote it.
	eventuallyBundles := func(names ...string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			for _, k := range Kinds {
				g.Expect(string(bundleOf(k))).To(Equal(string(cas(names...))), k.CRDName())
			}
		}, 10*time.Second, 50*time.Millisecond).Should(Succeed())
	}
	versions := func() []string {
		GinkgoHelper()
		out := make([]string, 0, len(Kinds))
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
			out = append(out, crd.ResourceVersion)
		}
		return out
	}
	newReplica := func(names ...string) *replica {
		GinkgoHelper()
		r := &replica{file: filepath.Join(GinkgoT().TempDir(), "ca.crt")}
		r.mount(names...)
		r.authorities = &Authorities{File: r.file, Secret: secret, Watcher: watcher, Interval: time.Hour}
		r.upgrader = &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service,
			Authorities: r.authorities, Interval: time.Hour}
		return r
	}

	BeforeAll(func() {
		installCRDs0_9()
		upgradeCRDs()
		var err error
		watcher, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		writeSecret("issued-1")

		runCtx, cancelRun := contextWithCancel()
		stop = cancelRun
		// Every replica reads ca.crt from its start, the leader or not.
		first, second = newReplica("issued-1"), newReplica("issued-1")
		first.run(runCtx, first.authorities.Start)
		second.run(runCtx, second.authorities.Start)
		// Only the leader runs the Upgrader.
		first.lead(runCtx)
	})
	AfterAll(func() {
		stop()
		first.wait()
		second.wait()
		secretObject := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secret.Namespace, Name: secret.Name}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secretObject))).To(Succeed())
		removeCRDs()
	})

	It("writes the CA once, the same from the mounted file and from the Secret", func() {
		eventuallyBundles("issued-1")
		before := versions()
		Consistently(versions, "500ms", "50ms").Should(Equal(before))
	})

	It("adds a new CA when the Secret changes, before any replica's mounted file has it", func() {
		writeSecret("issued-2")
		// Both mounts still hold issued-1, and so both replicas still serve under it; the
		// leader's next tick is an hour away.
		eventuallyBundles("issued-2", "issued-1")
	})

	It("does not write the CRDs again when the leader's mount catches up", func() {
		before := versions()
		first.mount("issued-2")
		Expect(first.upgrader.ConfigureConversion(ctx)).To(Succeed())
		Expect(versions()).To(Equal(before))
	})

	It("keeps both CAs when the standby takes the lease, which has read both itself", func() {
		// The standby's mount is still the old one: it serves under issued-1.
		before := versions()
		first.resign()
		second.lead(ctx)
		DeferCleanup(second.resign)
		Consistently(func(g Gomega) {
			for _, k := range Kinds {
				g.Expect(string(bundleOf(k))).To(Equal(string(cas("issued-2", "issued-1"))), k.CRDName())
			}
		}, "500ms", "50ms").Should(Succeed())
		Expect(versions()).To(Equal(before), "the new leader wrote the CRDs although nothing had changed")
	})

	It("keeps the CA of the mounted file while the Secret is gone, and takes the one it comes back with", func() {
		second.lead(ctx)
		DeferCleanup(second.resign)
		second.mount("issued-2")
		secretObject := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secret.Namespace, Name: secret.Name}}
		Expect(k8sClient.Delete(ctx, secretObject)).To(Succeed())
		Consistently(func(g Gomega) {
			for _, k := range Kinds {
				g.Expect(string(bundleOf(k))).To(Equal(string(cas("issued-2", "issued-1"))), k.CRDName())
			}
		}, "500ms", "50ms").Should(Succeed())

		writeSecret("issued-3")
		eventuallyBundles("issued-3", "issued-2", "issued-1")
	})

	It("does not keep a previous CA on a replica that started after the renewal and never read it", func() {
		// The price of trusting nothing but its own readings: until the mounts of the older
		// replicas are refreshed, they serve a certificate this leader does not vouch for.
		started := newReplica("issued-3")
		Expect(started.upgrader.ConfigureConversion(ctx)).To(Succeed())
		for _, k := range Kinds {
			Expect(string(bundleOf(k))).To(Equal(string(cas("issued-3"))), k.CRDName())
		}
	})
})

// replica is one operator pod: its mounted ca.crt, its readings of it, and the Upgrader it runs
// while it holds the lease.
type replica struct {
	file        string
	authorities *Authorities
	upgrader    *Upgrader
	done        []chan error
	resign      func()
}

// mount is the kubelet refreshing the replica's mounted Secret.
func (r *replica) mount(names ...string) {
	GinkgoHelper()
	Expect(os.WriteFile(r.file, cas(names...), 0o600)).To(Succeed())
}

func (r *replica) run(runCtx context.Context, start func(context.Context) error) {
	done := make(chan error, 1)
	r.done = append(r.done, done)
	go func() { done <- start(runCtx) }()
}

// lead runs the Upgrader, as the manager does for the replica that holds the lease, until
// resign.
func (r *replica) lead(parent context.Context) {
	leading, resign := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- r.upgrader.Start(leading) }()
	r.resign = func() {
		GinkgoHelper()
		resign()
		Eventually(done).Should(Receive(BeNil()))
	}
}

func (r *replica) wait() {
	GinkgoHelper()
	for _, done := range r.done {
		Eventually(done).Should(Receive(BeNil()))
	}
}

var _ = Describe("Authorities", func() {
	It("reads ca.crt on every replica, not only on the one that holds the lease", func() {
		// The manager starts a runnable that asks for leader election only once the replica
		// leads: a standby would then take over without ever having read the previous CA.
		Expect((&Authorities{}).NeedLeaderElection()).To(BeFalse())
		Expect((&Upgrader{}).NeedLeaderElection()).To(BeTrue(), "only the leader writes the CRDs")
	})

	It("refuses a ca.crt without a certificate, or with one that does not parse", func() {
		dir := GinkgoT().TempDir()
		a := &Authorities{File: filepath.Join(dir, "ca.crt")}
		_, err := a.Bundle()
		Expect(err).To(MatchError(ContainSubstring("no such file")))

		Expect(os.WriteFile(a.File, nil, 0o600)).To(Succeed())
		_, err = a.Bundle()
		Expect(err).To(MatchError(ContainSubstring("no certificate")))

		broken := bytes.Replace(testCA("broken"), []byte("MII"), []byte("AAA"), 1)
		Expect(os.WriteFile(a.File, broken, 0o600)).To(Succeed())
		_, err = a.Bundle()
		Expect(err).To(HaveOccurred())
	})

	It("writes each certificate once, without what is not a certificate", func() {
		a := &Authorities{File: filepath.Join(GinkgoT().TempDir(), "ca.crt")}
		content := append([]byte("# the CA of the webhooks\n"), cas("once", "once", "twice")...)
		Expect(os.WriteFile(a.File, content, 0o600)).To(Succeed())
		Expect(a.Bundle()).To(Equal(cas("once", "twice")))
	})

	It("says that ca.crt changed, once, and not when it is read again unchanged", func() {
		a := &Authorities{File: filepath.Join(GinkgoT().TempDir(), "ca.crt")}
		Expect(os.WriteFile(a.File, cas("signal-1"), 0o600)).To(Succeed())
		Expect(a.Bundle()).To(Equal(cas("signal-1")))
		Expect(a.Changed()).To(Receive())
		Expect(a.Bundle()).To(Equal(cas("signal-1")))
		Expect(a.Changed()).NotTo(Receive())

		Expect(os.WriteFile(a.File, cas("signal-2"), 0o600)).To(Succeed())
		Expect(a.Bundle()).To(Equal(cas("signal-2", "signal-1")))
		Expect(a.Changed()).To(Receive())
	})
})
