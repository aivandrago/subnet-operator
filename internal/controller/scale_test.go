//go:build scale

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

package controller

// The scale test measures one NetworkScope at the capacity the README promises for one
// instance: 100 accounts × 4 regions, discovery.concurrency 4, a 10-minute resync. AWS is an
// in-process fake that sleeps like EC2 does; the Kubernetes side is envtest's real
// kube-apiserver and etcd, reached through a manager's informer cache exactly as in
// production. It runs for about fifteen minutes, so it sits behind the "scale" build tag and
// runs with `make test-scale`. docs/operations/limits.md has the numbers it produced.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// scaleConfig is the shape of the run. The defaults are the documented capacity; every value
// can be overridden from the environment to try another shape without editing the test.
type scaleConfig struct {
	accounts    int
	regions     int
	concurrency int
	// callLatency is one EC2 Describe round trip. A target with discoverUnmanaged on (the
	// default) costs four of them, so 375ms makes the 1.5s per target the guidance assumes.
	callLatency time.Duration
	// throttledFraction of the targets are throttled in the throttling phase.
	throttledFraction float64
	// throttledHold is how long a throttled target keeps its discovery slot before the SDK
	// gives up: four backoff waits of rand×2^n seconds (n = 1..4) average 15s, at most 30s.
	throttledHold time.Duration
	// churnFraction of the subnets change their free-IP count between two syncs, the one
	// number that moves in a busy organization without anybody touching anything.
	churnFraction float64
}

func scaleConfigFromEnv() scaleConfig {
	c := scaleConfig{accounts: 100, regions: 4, concurrency: 4, callLatency: 375 * time.Millisecond,
		throttledFraction: 0.1, throttledHold: 15 * time.Second, churnFraction: 0.2}
	envInt := func(name string, v *int) {
		if s := os.Getenv(name); s != "" {
			n, err := strconv.Atoi(s)
			Expect(err).NotTo(HaveOccurred(), name)
			*v = n
		}
	}
	envDuration := func(name string, v *time.Duration) {
		if s := os.Getenv(name); s != "" {
			d, err := time.ParseDuration(s)
			Expect(err).NotTo(HaveOccurred(), name)
			*v = d
		}
	}
	envFloat := func(name string, v *float64) {
		if s := os.Getenv(name); s != "" {
			f, err := strconv.ParseFloat(s, 64)
			Expect(err).NotTo(HaveOccurred(), name)
			*v = f
		}
	}
	envInt("SCALE_ACCOUNTS", &c.accounts)
	envInt("SCALE_REGIONS", &c.regions)
	envInt("SCALE_CONCURRENCY", &c.concurrency)
	envDuration("SCALE_CALL_LATENCY", &c.callLatency)
	envDuration("SCALE_THROTTLED_HOLD", &c.throttledHold)
	envFloat("SCALE_THROTTLED_FRACTION", &c.throttledFraction)
	envFloat("SCALE_CHURN_FRACTION", &c.churnFraction)
	return c
}

var scaleRegions = []string{"eu-central-1", "eu-west-1", "us-east-1", "us-west-2", "ap-southeast-1", "sa-east-1"}

// scaleInventory builds the organization. Per target: 2–5 managed VPCs and 6–20 managed
// subnets, spread evenly over the indexes so the totals are stable (3.5 VPCs and 13 subnets
// on average). That is a landing zone as it usually looks: a VPC per environment plus a shared
// or inspection one, and subnets per availability zone and tier (3 AZs × 2–3 tiers per VPC,
// fewer in the small ones). On top, every target has the AWS default VPC with three default
// subnets, untagged and so unmanaged: they are discovered, counted and never mirrored, which
// is what the default discoverUnmanaged does with them. Every 25th target reuses 10.200.0.0/16
// for its first VPC, so a few overlaps are reported, as they are in any real organization.
func scaleInventory(c scaleConfig) (targets []inventory.TargetKey, snapshots map[inventory.TargetKey]*inventory.Snapshot) {
	snapshots = map[inventory.TargetKey]*inventory.Snapshot{}
	vpcIndex := 0
	for a := range c.accounts {
		account := fmt.Sprintf("%012d", 400000000000+a)
		for r := range c.regions {
			region := scaleRegions[r%len(scaleRegions)]
			i := len(targets)
			key := inventory.TargetKey{Account: account, Region: region}
			targets = append(targets, key)
			snap := &inventory.Snapshot{}
			nVPCs := 2 + (i*7)%4
			nSubnets := 6 + (i*11)%15
			for v := range nVPCs {
				cidr := fmt.Sprintf("10.%d.%d.0/20", (vpcIndex/16)%200, (vpcIndex%16)*16)
				if v == 0 && i%25 == 0 {
					cidr = "10.200.0.0/16"
				}
				vpcIndex++
				snap.Networks = append(snap.Networks, inventory.Network{
					ID: fmt.Sprintf("vpc-%s%s%02d", account[6:], regionCode(region), v), Account: account,
					Region: region, State: "available", CIDRBlocks: []string{cidr},
					Tags: map[string]string{
						"Name": fmt.Sprintf("workload-%s-%d", region, v), "hs/owner": fmt.Sprintf("team-%d", a%17),
						"hs/env":     []string{"prod", "staging", "dev", "shared", "inspection"}[v],
						"CostCenter": fmt.Sprintf("cc-%04d", a), "ManagedBy": "terraform",
					},
				})
			}
			for s := range nSubnets {
				vpc := snap.Networks[s%nVPCs]
				prefix := strings.Split(vpc.CIDRBlocks[0], ".")
				third, _ := strconv.Atoi(prefix[2])
				tags := map[string]string{
					"Name":      fmt.Sprintf("%s-%d", vpc.Tags["Name"], s),
					"hs/env":    vpc.Tags["hs/env"],
					"hs/tier":   []string{"public", "private", "data"}[s%3],
					"ManagedBy": "terraform", "CostCenter": vpc.Tags["CostCenter"],
				}
				// One subnet in ten was created by hand and never got an owner: the
				// requiredSubnetTags report has something to say.
				if s%10 != 9 {
					tags["hs/owner"] = vpc.Tags["hs/owner"]
				}
				snap.Subnets = append(snap.Subnets, inventory.Subnet{
					ID: fmt.Sprintf("subnet-%s%s%02d", account[6:], regionCode(region), s), NetworkID: vpc.ID,
					Account: account, Region: region, State: "available",
					CIDRBlock: fmt.Sprintf("%s.%s.%d.0/24", prefix[0], prefix[1], third+s/nVPCs),
					Zone:      region + string(rune('a'+s%3)),
					TotalIPs:  new(int64(251)), AvailableIPs: new(int64(100 + s)),
					OwnershipSource: networkv1.OwnershipSourceSubnet,
					AWS: &networkv1.AWSSubnetStatus{AvailabilityZoneID: fmt.Sprintf("az%d", s%3+1), Public: s%3 == 0,
						RouteTableID: fmt.Sprintf("rtb-%s%s%02d", account[6:], regionCode(region), s%nVPCs)},
					Tags: tags,
				})
			}
			def := inventory.Network{ID: fmt.Sprintf("vpc-%s%sdf", account[6:], regionCode(region)), Account: account,
				Region: region, State: "available", CIDRBlocks: []string{"172.31.0.0/16"},
				AWS: &networkv1.AWSNetworkStatus{IsDefault: true}}
			snap.UnmanagedNetworks = []inventory.Network{def}
			for z := range 3 {
				snap.UnmanagedSubnets = append(snap.UnmanagedSubnets, inventory.Subnet{
					ID: fmt.Sprintf("subnet-%s%sd%d", account[6:], regionCode(region), z), NetworkID: def.ID,
					Account: account, Region: region, State: "available",
					CIDRBlock: fmt.Sprintf("172.31.%d.0/20", z*16), TotalIPs: new(int64(4091)), AvailableIPs: new(int64(4091)),
				})
			}
			snapshots[key] = snap
		}
	}
	return targets, snapshots
}

func regionCode(region string) string {
	parts := strings.Split(region, "-")
	return parts[0][:1] + parts[1][:1] + parts[2]
}

// scaleDiscoverer is EC2 as the operator sees it: every Describe call takes callLatency, and a
// throttled target holds its slot for the SDK's retries before it gives up with ErrThrottled.
// It returns a copy of the target's snapshot, as the real discoverer builds a new one per call.
type scaleDiscoverer struct {
	cfg       scaleConfig
	mu        sync.Mutex
	snapshots map[inventory.TargetKey]*inventory.Snapshot
	throttled map[inventory.TargetKey]bool
	calls     map[inventory.TargetKey]int
	// first and last bracket the discovery phase of a sync: from the first call in to the
	// last one out.
	first, last time.Time
}

func (d *scaleDiscoverer) Discover(ctx context.Context, t inventory.Target) (*inventory.Snapshot, error) {
	d.mu.Lock()
	if d.first.IsZero() {
		d.first = time.Now()
	}
	d.calls[t.Key()]++
	throttled := d.throttled[t.Key()]
	snap := d.snapshots[t.Key()]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.last = time.Now()
		d.mu.Unlock()
	}()

	calls := 3
	if t.DiscoverUnmanaged {
		calls = 4
	}
	wait := time.Duration(calls) * d.cfg.callLatency
	if throttled {
		// DescribeVpcs is the first call and the one that gives up: five attempts, four waits.
		wait = 5*d.cfg.callLatency + d.cfg.throttledHold
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if throttled {
		return nil, throttledError()
	}
	return copySnapshot(snap), nil
}

func (d *scaleDiscoverer) resetPhase() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.first, d.last = time.Time{}, time.Time{}
	d.calls = map[inventory.TargetKey]int{}
}

func (d *scaleDiscoverer) discovered() (targets int, wall time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls), d.last.Sub(d.first)
}

func copySnapshot(s *inventory.Snapshot) *inventory.Snapshot {
	out := &inventory.Snapshot{Networks: slices.Clone(s.Networks), Subnets: slices.Clone(s.Subnets),
		UnmanagedNetworks: slices.Clone(s.UnmanagedNetworks), UnmanagedSubnets: slices.Clone(s.UnmanagedSubnets)}
	return out
}

// requestCounter counts what the operator sends to the API server, by verb and resource. It
// wraps the manager's transport, so it sees exactly the requests production would make:
// informer-cache reads never reach it, uncached lists and every write do.
type requestCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *requestCounter) wrap(rt http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		key := req.Method + " " + resourceOf(req.URL.Path)
		if req.URL.Query().Get("watch") == "true" {
			key = "WATCH " + resourceOf(req.URL.Path)
		}
		c.mu.Lock()
		c.counts[key]++
		c.mu.Unlock()
		return rt.RoundTrip(req)
	})
}

func (c *requestCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = map[string]int{}
}

func (c *requestCounter) get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

// writes is every create, update, patch and delete except Events, which are best effort and
// batched by the recorder.
func (c *requestCounter) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, v := range c.counts {
		if !strings.HasPrefix(k, "GET ") && !strings.HasPrefix(k, "WATCH ") && !strings.HasSuffix(k, " events") {
			n += v
		}
	}
	return n
}

func (c *requestCounter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.counts))
	for k := range c.counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c.counts[k]))
	}
	return strings.Join(parts, " ")
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// resourceOf turns /apis/network.hypersurgery.dev/v1beta1/subnets/subnet-1/status into
// "subnets/status" and a list into "subnets".
func resourceOf(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// /apis/<group>/<version>/[namespaces/<ns>/]<resource>[/<name>[/<subresource>]]
	switch {
	case len(parts) > 2 && parts[0] == "api":
		parts = parts[2:]
	case len(parts) > 3 && parts[0] == "apis":
		parts = parts[3:]
	default:
		return path // discovery
	}
	if len(parts) > 2 && parts[0] == "namespaces" {
		parts = parts[2:]
	}
	switch len(parts) {
	case 3:
		return parts[0] + "/" + parts[2]
	default:
		return parts[0]
	}
}

// resourceSampler records the peak heap in use and the peak number of goroutines of the
// process while a sync runs. The API server and etcd are separate processes, so what it sees
// is the operator: its informer cache, the uncached lists and the snapshots of the sync.
type resourceSampler struct {
	stop                           chan struct{}
	done                           chan struct{}
	peakHeap, peakRoutine, peakRSS atomic.Uint64
}

// residentBytes is the process's resident set from /proc (Linux only; 0 elsewhere). It is what
// a container's memory limit is measured against, heap or not: stacks, runtime overhead and
// memory the garbage collector has not returned yet all count.
func residentBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}

func startSampler() *resourceSampler {
	s := &resourceSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapInuse > s.peakHeap.Load() {
				s.peakHeap.Store(m.HeapInuse)
			}
			if g := uint64(runtime.NumGoroutine()); g > s.peakRoutine.Load() {
				s.peakRoutine.Store(g)
			}
			if rss := residentBytes(); rss > s.peakRSS.Load() {
				s.peakRSS.Store(rss)
			}
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *resourceSampler) finish() (heapMiB, rssMiB float64, goroutines uint64) {
	close(s.stop)
	<-s.done
	return float64(s.peakHeap.Load()) / (1 << 20), float64(s.peakRSS.Load()) / (1 << 20), s.peakRoutine.Load()
}

// cpuTime is the CPU the process has used so far, user and system. The API server and etcd
// are child processes and not counted, so the difference over a sync is the operator's share,
// to compare with the chart's CPU limit.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func heapAfterGC() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapInuse) / (1 << 20)
}

var _ = Describe("NetworkScope at the documented capacity", Label("scale"), Ordered, func() {
	const scopeName = "scale"
	const resync = 10 * time.Minute

	var (
		shape      scaleConfig
		keys       []inventory.TargetKey
		snapshots  map[inventory.TargetKey]*inventory.Snapshot
		discoverer *scaleDiscoverer
		counter    *requestCounter
		reconciler *NetworkScopeReconciler
		clk        *clocktesting.FakeClock
		mgrCancel  context.CancelFunc
		nVPCs      int
		nSubnets   int
		report     []string
	)

	type phase struct {
		reconcile, discovery time.Duration
		discovered           int
	}

	// run reconciles the scope once, at the fake clock moved by step: the clock decides whether
	// the reconcile is a full sync, the stopwatch measures it in real time.
	run := func(name string, step time.Duration) phase {
		GinkgoHelper()
		clk.Step(step)
		discoverer.resetPhase()
		counter.reset()
		sampler := startSampler()
		cpuStart := cpuTime()
		start := time.Now()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		elapsed := time.Since(start)
		cpu := cpuTime() - cpuStart
		heap, rss, routines := sampler.finish()
		Expect(err).NotTo(HaveOccurred())
		p := phase{reconcile: elapsed}
		p.discovered, p.discovery = discoverer.discovered()
		line := fmt.Sprintf("%-34s reconcile %6.1fs  discovery %6.1fs (%d targets)  API %6.1fs  CPU %5.1fs  "+
			"writes %5d  peak heap %6.1f MiB  peak RSS %6.1f MiB  peak goroutines %4d\n    requests: %s",
			name, elapsed.Seconds(), p.discovery.Seconds(), p.discovered, (elapsed - p.discovery).Seconds(),
			cpu.Seconds(), counter.writes(), heap, rss, routines, counter)
		report = append(report, line)
		GinkgoWriter.Println(line)
		return p
	}

	getScope := func() *networkv1.NetworkScope {
		GinkgoHelper()
		s := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		return s
	}

	BeforeAll(func() {
		shape = scaleConfigFromEnv()
		keys, snapshots = scaleInventory(shape)
		for _, s := range snapshots {
			nVPCs += len(s.Networks)
			nSubnets += len(s.Subnets)
		}
		report = append(report, fmt.Sprintf("%d targets (%d accounts × %d regions), %d VPCs, %d subnets, "+
			"concurrency %d, %v per EC2 call, GOMAXPROCS %d, GOMEMLIMIT %s",
			len(keys), shape.accounts, shape.regions, nVPCs, nSubnets, shape.concurrency, shape.callLatency,
			runtime.GOMAXPROCS(0), cmp.Or(os.Getenv("GOMEMLIMIT"), "unset")))

		discoverer = &scaleDiscoverer{cfg: shape, snapshots: snapshots, throttled: map[inventory.TargetKey]bool{},
			calls: map[inventory.TargetKey]int{}}
		counter = &requestCounter{counts: map[string]int{}}

		// A manager like the one cmd/main.go builds: cached client for gets, API reader for
		// the lists that must see this sync's own creates, and an event recorder.
		mgrCfg := rest.CopyConfig(cfg)
		mgrCfg.WrapTransport = counter.wrap
		// What cmd/main.go gets from ctrl.GetConfigOrDie: no client-side rate limit, the API
		// server's priority and fairness decides. envtest's own config allows 1000 QPS.
		mgrCfg.QPS = -1
		mgr, err := ctrl.NewManager(mgrCfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

		clk = clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
		reconciler = &NetworkScopeReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
			Scheme: mgr.GetScheme(), Providers: awsProviders(discoverer, nil, nil), Concurrency: shape.concurrency, Clock: clk,
			Recorder: mgr.GetEventRecorder("network.hypersurgery.dev/networkscope")}
		// The top of each delay, so the retry below lands exactly when the backoff says.
		reconciler.backoff.jitter = func() float64 { return 0.999999 }

		accounts := make([]networkv1.Account, 0, shape.accounts)
		regions := make([]string, 0, shape.regions)
		for r := range shape.regions {
			regions = append(regions, scaleRegions[r%len(scaleRegions)])
		}
		for a := range shape.accounts {
			id := fmt.Sprintf("%012d", 400000000000+a)
			accounts = append(accounts, networkv1.Account{ID: id,
				AWS: &networkv1.AWSAccount{RoleARN: fmt.Sprintf("arn:aws:iam::%s:role/subnet-inventory-read", id)}})
		}
		Expect(k8sClient.Create(ctx, &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderAWS,
				NamespaceSelector: &metav1.LabelSelector{},
				Accounts:          accounts, Regions: regions,
				NetworkSelector:    &networkv1.NetworkSelector{MatchTags: map[string]string{"hs/owner": ""}},
				RequiredSubnetTags: []string{"hs/owner"},
				ResyncInterval:     &metav1.Duration{Duration: resync},
			},
		})).To(Succeed())
	})

	AfterAll(func() {
		mgrCancel()
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getScope())).To(Succeed())
		AddReportEntry("scale", strings.Join(report, "\n"))
		_, _ = fmt.Fprintln(os.Stdout, "\n=== scale test results ===\n"+strings.Join(report, "\n"))
	})

	It("mirrors the whole organization on the first sync", func() {
		p := run("first full sync (empty cluster)", 0)
		Expect(p.discovered).To(Equal(len(keys)))
		Expect(counter.get("POST vpcs")).To(Equal(nVPCs))
		Expect(counter.get("POST subnets")).To(Equal(nSubnets))
		scope := getScope()
		Expect(scope.Status.Networks).To(Equal(int32(nVPCs)))
		Expect(scope.Status.Subnets).To(Equal(int32(nSubnets)))
		Expect(p.reconcile).To(BeNumerically("<", resync), "a full sync must fit in the resync interval")
		report = append(report, fmt.Sprintf("    retained heap after GC: %.1f MiB", heapAfterGC()))
	})

	It("writes nothing but the scope's status when nothing changed", func() {
		p := run("full sync, nothing changed", resync)
		Expect(p.discovered).To(Equal(len(keys)))
		Expect(counter.writes()).To(Equal(1), "only the scope's lastSyncTime moves: "+counter.String())
		Expect(counter.get("PUT networkscopes/status")).To(Equal(1))
		Expect(p.reconcile).To(BeNumerically("<", resync))
		report = append(report, fmt.Sprintf("    retained heap after GC: %.1f MiB", heapAfterGC()))
		// SCALE_HEAP_PROFILE=<file> keeps a heap profile of the steady state, for `go tool pprof`.
		if path := os.Getenv("SCALE_HEAP_PROFILE"); path != "" {
			f, err := os.Create(path)
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(f.Close()).To(Succeed()) }()
			Expect(pprof.WriteHeapProfile(f)).To(Succeed())
		}
	})

	It("writes exactly the objects whose numbers moved", func() {
		// Free IPs move in a fraction of the subnets; each one moves its VPC's totals too.
		changedSubnets, changedVPCs := 0, map[string]bool{}
		discoverer.mu.Lock()
		for _, k := range keys {
			snap := discoverer.snapshots[k]
			for i := range snap.Subnets {
				if float64((i*37+len(k.Account))%100) < shape.churnFraction*100 {
					snap.Subnets[i].AvailableIPs = new(*snap.Subnets[i].AvailableIPs - 1)
					changedSubnets++
					changedVPCs[snap.Subnets[i].NetworkID] = true
				}
			}
		}
		discoverer.mu.Unlock()

		run(fmt.Sprintf("full sync, %.0f%% of subnets churned", shape.churnFraction*100), resync)
		Expect(counter.get("PUT subnets/status")).To(Equal(changedSubnets))
		Expect(counter.get("PUT vpcs/status")).To(Equal(len(changedVPCs)))
		Expect(counter.get("PUT subnets")+counter.get("PUT vpcs")).To(BeZero(), "spec and labels did not change")
		Expect(counter.writes()).To(Equal(changedSubnets + len(changedVPCs) + 1))
	})

	It("keeps syncing the healthy targets while a tenth of them is throttled", func() {
		nThrottled := int(float64(len(keys)) * shape.throttledFraction)
		discoverer.mu.Lock()
		// Spread over the list rather than bunched at one end, as throttling would be.
		for i := 0; i < len(keys) && len(discoverer.throttled) < nThrottled; i += len(keys) / max(nThrottled, 1) {
			discoverer.throttled[keys[i]] = true
		}
		discoverer.mu.Unlock()

		p := run(fmt.Sprintf("full sync, %d targets throttled", nThrottled), resync)
		Expect(p.discovered).To(Equal(len(keys)))
		scope := getScope()
		healthy, busy := 0, 0
		for _, t := range scope.Status.Targets {
			if t.Error == "" {
				Expect(t.LastSyncTime.Time).To(BeTemporally("==", clk.Now().Truncate(time.Second)))
				healthy++
			} else {
				Expect(t.Error).To(HavePrefix(inventory.ErrThrottled.Error()))
				busy++
			}
		}
		Expect(healthy).To(Equal(len(keys)-nThrottled), "every healthy target synced in the same pass")
		Expect(busy).To(Equal(nThrottled))
		Expect(scope.Status.Networks).To(Equal(int32(nVPCs)), "throttled targets keep their objects")
		Expect(counter.get("DELETE subnets") + counter.get("DELETE vpcs")).To(BeZero())
		Expect(p.reconcile).To(BeNumerically("<", resync))

		By("retrying only the throttled targets once their backoff runs out")
		discoverer.mu.Lock()
		discoverer.throttled = map[inventory.TargetKey]bool{}
		discoverer.mu.Unlock()
		p = run("retry of the throttled targets alone", throttleBackoffBase)
		Expect(p.discovered).To(Equal(nThrottled), "the healthy targets wait for their full sync")
		for _, t := range getScope().Status.Targets {
			Expect(t.Error).To(BeEmpty())
		}
		Expect(counter.writes()).To(Equal(1), "their objects were current already: "+counter.String())
	})

	It("syncs only the targets an event names between full syncs", func() {
		named := keys[:10]
		Expect(reconciler.NotifyChanged(ctx, named)).To(Succeed())
		p := run("event-driven sync of 10 targets", 10*time.Second)
		Expect(p.discovered).To(Equal(len(named)))
	})
})
