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

	MaxRate           float64       // requests per second, at any moment
	MaxDuration       time.Duration // how long one run, or one trial of a search, lasts
	MaxWorkers        int           // requests in flight, which is also connections
	MaxTimeout        time.Duration // how long one request, gRPC stream included, may take
	MaxStreamMessages int           // messages one streaming call may send or read

	// AllowWriteScenarios lets a journey scenario declare mutating methods
	// (POST, PUT, PATCH, DELETE). Off by default: a journey an agent reads
	// and drives should not change anything unless the operator said so.
	AllowWriteScenarios bool
	// AllowRawJourneyYAML, with a scenario directory, lets the server read a
	// scenario by absolute path instead of only by name in the directory.
	AllowRawJourneyYAML bool

	// lookup resolves a host name; tests replace it.
	lookup func(context.Context, string) ([]net.IP, error)
}

// Defaults for the limits, chosen so that an agent that is not told anything
// cannot hurt a small service: they are raised with the -max-* flags.
const (
	DefaultMaxRate           = 1000
	DefaultMaxDuration       = 2 * time.Minute
	DefaultMaxWorkers        = 200
	DefaultMaxTimeout        = time.Minute
	DefaultMaxStreamMessages = 100
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
	if p.MaxTimeout <= 0 {
		p.MaxTimeout = DefaultMaxTimeout
	}
	if p.MaxStreamMessages <= 0 {
		p.MaxStreamMessages = DefaultMaxStreamMessages
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

// CheckJourney says whether a journey is within the limits. The effective
// request rate is the session rate times the step count: a short script can
// multiply a modest session rate into a large write load.
func (p Policy) CheckJourney(sessionRate float64, steps int, duration time.Duration, workers int) error {
	return p.CheckLoad(sessionRate*float64(steps), duration, workers)
}

// CheckWriteMethod says whether a journey step may use a mutating method
// (POST, PUT, PATCH or DELETE). It is refused for an agent unless the
// operator enabled write scenarios; the refusal names the setting to change,
// as the other refusals do.
func (p Policy) CheckWriteMethod(method string) error {
	if p.withDefaults().AllowWriteScenarios {
		return nil
	}
	return fmt.Errorf("%s changes state, and write scenarios are not enabled: the operator can enable them with AllowWriteScenarios", strings.ToUpper(method))
}

// CheckTimeout says whether one request may take this long. Zero means the
// default and is fine; a negative duration never is.
func (p Policy) CheckTimeout(timeout time.Duration) error {
	p = p.withDefaults()
	switch {
	case timeout < 0:
		return fmt.Errorf("timeout %v is negative: give a positive duration such as 5s", timeout)
	case timeout > p.MaxTimeout:
		return fmt.Errorf("timeout %v is over this server's limit of %v: it can be raised with -max-timeout", timeout, p.MaxTimeout)
	}
	return nil
}

// CheckStreamMessages says whether one streaming call may send and read this
// many messages. A streaming call is one scheduled request, so the message
// counts are a separate ceiling from the request rate.
func (p Policy) CheckStreamMessages(send, recv int) error {
	p = p.withDefaults()
	switch {
	case send < 0 || recv < 0:
		return fmt.Errorf("send_messages and recv_messages must not be negative, got %d and %d", send, recv)
	case send > p.MaxStreamMessages:
		return fmt.Errorf("send_messages %d is over this server's limit of %d: it can be raised with -max-stream-messages", send, p.MaxStreamMessages)
	case recv > p.MaxStreamMessages:
		return fmt.Errorf("recv_messages %d is over this server's limit of %d: it can be raised with -max-stream-messages", recv, p.MaxStreamMessages)
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
	case p.MaxTimeout < 0:
		return fmt.Errorf("the timeout limit cannot be negative")
	case p.MaxStreamMessages < 0:
		return fmt.Errorf("the stream messages limit cannot be negative")
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
	msg := fmt.Sprintf("targets: %s; at most %.0f requests/s, %v per run, %d workers, %v per request, %d messages per stream", hosts, p.MaxRate, p.MaxDuration, p.MaxWorkers, p.MaxTimeout, p.MaxStreamMessages)
	if p.AllowWriteScenarios {
		msg += "; write scenarios enabled"
	}
	if p.AllowRawJourneyYAML {
		msg += "; raw journey YAML allowed"
	}
	return msg
}
