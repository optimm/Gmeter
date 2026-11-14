package mcpserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeLookup answers name lookups from a table, so no test needs a network.
func fakeLookup(table map[string][]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		addrs, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

func TestLoopbackIsAllowedWithoutAnyConfiguration(t *testing.T) {
	p := Policy{lookup: fakeLookup(map[string][]string{"localhost": {"127.0.0.1", "::1"}})}
	for _, target := range []string{"http://127.0.0.1:8080", "http://localhost:3000/x", "https://[::1]:9", "127.0.0.1:50051", "localhost:50051"} {
		if err := p.CheckTarget(context.Background(), target); err != nil {
			t.Errorf("CheckTarget(%q) = %v, want it allowed", target, err)
		}
	}
}

func TestAHostThatWasNotAllowedIsRefusedWithTheWayToAllowIt(t *testing.T) {
	p := Policy{lookup: fakeLookup(map[string][]string{"api.example.com": {"93.184.216.34"}})}
	err := p.CheckTarget(context.Background(), "https://api.example.com/v1")
	if err == nil || !strings.Contains(err.Error(), "api.example.com") || !strings.Contains(err.Error(), "-allow") {
		t.Fatalf("err = %v, want a refusal naming the host and the -allow flag", err)
	}
}

func TestAllowedHostsMatchByNameSuffixAndNetwork(t *testing.T) {
	p := Policy{
		Allow: []string{"api.example.com", "*.staging.example.com", "10.1.0.0/16"},
		lookup: fakeLookup(map[string][]string{
			"api.example.com":         {"93.184.216.34"},
			"web.staging.example.com": {"93.184.216.35"},
			"other.example.com":       {"93.184.216.36"},
		}),
	}
	for target, want := range map[string]bool{
		"https://api.example.com":         true,
		"https://web.staging.example.com": true,
		"https://staging.example.com":     false, // the wildcard is for subdomains
		"https://other.example.com":       false,
		"http://10.1.2.3:80":              true,
		"http://10.2.0.1:80":              false,
	} {
		got := p.CheckTarget(context.Background(), target) == nil
		if got != want {
			t.Errorf("CheckTarget(%q) allowed = %v, want %v", target, got, want)
		}
	}
}

func TestCloudMetadataAddressesAreRefusedEvenWhenEverythingIsAllowed(t *testing.T) {
	p := Policy{
		Allow: []string{"*"},
		lookup: fakeLookup(map[string][]string{
			"metadata.google.internal": {"169.254.169.254"},
			"sneaky.example.com":       {"93.184.216.34", "169.254.169.254"}, // a name that points at it
		}),
	}
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1",
		"http://sneaky.example.com",
		"http://[fd00:ec2::254]/",
		"http://169.254.1.1",
	} {
		err := p.CheckTarget(context.Background(), target)
		if err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Errorf("CheckTarget(%q) = %v, want a refusal that says why", target, err)
		}
	}
}

func TestAWildcardAllowsAnyOtherHost(t *testing.T) {
	p := Policy{Allow: []string{"*"}, lookup: fakeLookup(map[string][]string{"api.example.com": {"93.184.216.34"}})}
	if err := p.CheckTarget(context.Background(), "https://api.example.com"); err != nil {
		t.Errorf("err = %v, want any host allowed by *", err)
	}
}

func TestATargetThatDoesNotResolveIsReportedAsThat(t *testing.T) {
	p := Policy{Allow: []string{"*"}, lookup: fakeLookup(nil)}
	err := p.CheckTarget(context.Background(), "http://nope.invalid")
	if err == nil || !strings.Contains(err.Error(), "nope.invalid") {
		t.Errorf("err = %v, want it to name the host that did not resolve", err)
	}
}

func TestLimitsCapTheLoadAnAgentMayAsk(t *testing.T) {
	p := Policy{MaxRate: 500, MaxDuration: 30 * time.Second, MaxWorkers: 100}
	for name, c := range map[string]struct {
		rate     float64
		duration time.Duration
		workers  int
		want     string
	}{
		"rate":     {600, time.Second, 10, "rate"},
		"duration": {10, time.Minute, 10, "duration"},
		"workers":  {10, time.Second, 200, "workers"},
	} {
		err := p.CheckLoad(c.rate, c.duration, c.workers)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "-max-") {
			t.Errorf("%s: err = %v, want a refusal that names the limit and the flag", name, err)
		}
	}
	if err := p.CheckLoad(500, 30*time.Second, 100); err != nil {
		t.Errorf("at the limits: err = %v, want allowed", err)
	}
}

func TestDefaultsAreSetWhereTheCallerLeftThemZero(t *testing.T) {
	p := Policy{}.withDefaults()
	if p.MaxRate <= 0 || p.MaxDuration <= 0 || p.MaxWorkers <= 0 || p.lookup == nil {
		t.Errorf("withDefaults() = %+v, want every limit set", p)
	}
}

func TestValidateRefusesWhatCouldNeverWork(t *testing.T) {
	for name, p := range map[string]Policy{
		"bad cidr":       {Allow: []string{"10.0.0.0/99"}},
		"empty entry":    {Allow: []string{" "}},
		"negative rate":  {MaxRate: -1},
		"negative time":  {MaxDuration: -time.Second},
		"negative count": {MaxWorkers: -3},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := (Policy{Allow: []string{"*", "a.example", "*.b.example", "10.0.0.0/8", "::1"}}).Validate(); err != nil {
		t.Errorf("a good policy: %v", err)
	}
}

func TestDescribeSaysWhatThePolicyAllows(t *testing.T) {
	got := Policy{Allow: []string{"api.test"}, MaxRate: 250}.Describe()
	for _, want := range []string{"loopback", "api.test", "250", "2m0s", "200"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}
