// Package mcpserver serves Gmeter to an AI agent over the Model Context
// Protocol, on top of the public gmeter package.
package mcpserver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Policy is what an agent may do. Sending load is not harmless, and the agent
// that asks for it is not the person who owns the target, so everything that is
// not loopback must be allowed by name, and the load has a ceiling.
type Policy struct {
	// Allow lists the hosts, besides loopback, that may be targeted: a host name,
	// "*.example.com" for its subdomains, an IP address, a CIDR range such as
	// "10.0.0.0/8", or "*" for any host. Cloud metadata addresses are refused
	// whatever this says.
	Allow []string

	MaxRate     float64       // requests per second, at any moment
	MaxDuration time.Duration // how long one run, or one trial of a search, lasts
	MaxWorkers  int           // requests in flight, which is also connections

	// lookup resolves a host name; tests replace it.
	lookup func(context.Context, string) ([]net.IP, error)
}

// Defaults for the limits, chosen so that an agent that is not told anything
// cannot hurt a small service: they are raised with the -max-* flags.
const (
	DefaultMaxRate     = 1000
	DefaultMaxDuration = 2 * time.Minute
	DefaultMaxWorkers  = 200
)

func (p Policy) withDefaults() Policy {
	if p.MaxRate <= 0 {
		p.MaxRate = DefaultMaxRate
	}
	if p.MaxDuration <= 0 {
		p.MaxDuration = DefaultMaxDuration
	}
	if p.MaxWorkers <= 0 {
		p.MaxWorkers = DefaultMaxWorkers
	}
	if p.lookup == nil {
		p.lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, len(addrs))
			for i, a := range addrs {
				ips[i] = a.IP
			}
			return ips, err
		}
	}
	return p
}

// metadataNames are the names cloud providers serve instance credentials on.
var metadataNames = map[string]bool{
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// isMetadata reports whether ip is where a cloud's metadata service lives: the
// link-local range (169.254.0.0/16, which holds the usual 169.254.169.254) and
// AWS's IPv6 address.
func isMetadata(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.Equal(net.ParseIP("fd00:ec2::254"))
}

// hostOf returns the host of a target, which is a URL for HTTP and host:port for
// gRPC.
func hostOf(target string) (string, error) {
	if !strings.Contains(target, "://") {
		target = "//" + target
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%q is not a URL or host:port", target)
	}
	return u.Hostname(), nil
}

// CheckTarget says whether an agent may send load to target, with the way to
// allow it if not. The host is resolved, so that a name that points at a
// forbidden address is refused as the address would be.
func (p Policy) CheckTarget(ctx context.Context, target string) error {
	p = p.withDefaults()
	host, err := hostOf(target)
	if err != nil {
		return err
	}
	lower := strings.ToLower(host)
	if metadataNames[lower] {
		return fmt.Errorf("refusing %s: it is a cloud metadata service, which holds credentials", host)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = p.lookup(ctx, host); err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isMetadata(ip) {
			return fmt.Errorf("refusing %s: it is, or resolves to, %s, a cloud metadata address", host, ip)
		}
	}
	if allLoopback(lower, ips) || p.allows(lower, ips) {
		return nil
	}
	return fmt.Errorf("%s is not allowed: an agent may only target loopback unless the server was started with -allow %s", host, host)
}

func allLoopback(host string, ips []net.IP) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return len(ips) > 0
}

func (p Policy) allows(host string, ips []net.IP) bool {
	for _, a := range p.Allow {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "*" || a == host:
			return true
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(host, a[1:]) {
				return true
			}
		case strings.Contains(a, "/"):
			if _, n, err := net.ParseCIDR(a); err == nil {
				for _, ip := range ips {
					if n.Contains(ip) {
						return true
					}
				}
			}
		}
	}
	return false
}

// CheckLoad says whether a load is within the limits. rate is the highest
// request rate it reaches.
func (p Policy) CheckLoad(rate float64, duration time.Duration, workers int) error {
	p = p.withDefaults()
	switch {
	case rate > p.MaxRate:
		return fmt.Errorf("rate %.0f/s is over this server's limit of %.0f/s: it can be raised with -max-rate", rate, p.MaxRate)
	case duration > p.MaxDuration:
		return fmt.Errorf("duration %v is over this server's limit of %v: it can be raised with -max-duration", duration, p.MaxDuration)
	case workers > p.MaxWorkers:
		return fmt.Errorf("workers %d is over this server's limit of %d: it can be raised with -max-workers", workers, p.MaxWorkers)
	}
	return nil
}

// Validate refuses a policy that could never work as written: an allow entry
// that is a malformed range, or a negative limit. A zero limit is fine, it takes
// the default.
func (p Policy) Validate() error {
	for _, a := range p.Allow {
		a = strings.TrimSpace(a)
		if a == "" {
			return fmt.Errorf("an empty entry in the allow list")
		}
		if strings.Contains(a, "/") {
			if _, _, err := net.ParseCIDR(a); err != nil {
				return fmt.Errorf("allow %q is not a CIDR range such as 10.0.0.0/8", a)
			}
		}
	}
	switch {
	case p.MaxRate < 0:
		return fmt.Errorf("the rate limit cannot be negative")
	case p.MaxDuration < 0:
		return fmt.Errorf("the duration limit cannot be negative")
	case p.MaxWorkers < 0:
		return fmt.Errorf("the workers limit cannot be negative")
	}
	return nil
}

// Describe says what the policy allows, for the operator to read at start-up.
func (p Policy) Describe() string {
	p = p.withDefaults()
	hosts := "loopback only"
	if len(p.Allow) > 0 {
		hosts = "loopback and " + strings.Join(p.Allow, ", ")
	}
	return fmt.Sprintf("targets: %s; at most %.0f requests/s, %v per run, %d workers", hosts, p.MaxRate, p.MaxDuration, p.MaxWorkers)
}
