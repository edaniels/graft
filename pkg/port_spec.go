package graft

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// PortForwardSpec describes an explicit port forward: which remote port to forward,
// which local port to bind, and the protocol (tcp or udp).
type PortForwardSpec struct {
	RemotePort uint32
	LocalPort  uint32 // same as RemotePort when not explicitly mapped
	Protocol   string // "tcp" or "udp"
}

// portSpecPattern matches strings like "8080", "3000:8080", "8080/tcp", "3000:8080/udp".
var portSpecPattern = regexp.MustCompile(`^\d+(:\d+)?/(tcp|udp)$|^\d+(:\d+)?$`)

// IsPortSpec returns true if s looks like a port forward spec rather than a command name.
func IsPortSpec(s string) bool {
	if s == "" {
		return false
	}

	// Must match pattern: digits, optional :digits, optional /protocol.
	// Quick check: first char must be a digit.
	if s[0] < '0' || s[0] > '9' {
		return false
	}

	return portSpecPattern.MatchString(s)
}

// ParsePortSpec parses a port spec string into a PortForwardSpec.
// Format: [local_port:]remote_port[/protocol].
func ParsePortSpec(s string) (PortForwardSpec, error) {
	if !IsPortSpec(s) {
		return PortForwardSpec{}, errors.Errorf("invalid port spec %q", s)
	}

	spec := PortForwardSpec{Protocol: ProtocolTCP}

	// Split off protocol suffix.
	portPart := s
	if idx := strings.LastIndex(s, "/"); idx != -1 {
		proto := s[idx+1:]
		if proto != ProtocolTCP && proto != ProtocolUDP {
			return PortForwardSpec{}, errors.Errorf("invalid protocol %q in port spec %q; must be tcp or udp", proto, s)
		}

		spec.Protocol = proto
		portPart = s[:idx]
	}

	// Split local:remote.
	if before, after, ok := strings.Cut(portPart, ":"); ok {
		localStr := before
		remoteStr := after

		local, err := parsePort(localStr, s)
		if err != nil {
			return PortForwardSpec{}, err
		}

		remote, err := parsePort(remoteStr, s)
		if err != nil {
			return PortForwardSpec{}, err
		}

		spec.LocalPort = local
		spec.RemotePort = remote
	} else {
		port, err := parsePort(portPart, s)
		if err != nil {
			return PortForwardSpec{}, err
		}

		spec.LocalPort = port
		spec.RemotePort = port
	}

	return spec, nil
}

func parsePort(s, fullSpec string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errors.Errorf("invalid port number %q in port spec %q", s, fullSpec)
	}

	if n == 0 || n > 65535 {
		return 0, errors.Errorf("port %d out of range (1-65535) in port spec %q", n, fullSpec)
	}

	return uint32(n), nil
}

// EffectiveProtocol returns the protocol, defaulting to "tcp" if empty.
func (s PortForwardSpec) EffectiveProtocol() string {
	if s.Protocol == "" {
		return ProtocolTCP
	}

	return s.Protocol
}

// EffectiveLocalPort returns the local port, defaulting to RemotePort if zero.
func (s PortForwardSpec) EffectiveLocalPort() uint32 {
	if s.LocalPort == 0 {
		return s.RemotePort
	}

	return s.LocalPort
}

// PortForwardSpecFromProto converts a proto ExplicitPortForwardSpec to a PortForwardSpec.
func PortForwardSpecFromProto(p *graftv1.ExplicitPortForwardSpec) PortForwardSpec {
	return PortForwardSpec{
		RemotePort: p.GetRemotePort(),
		LocalPort:  p.GetLocalPort(),
		Protocol:   p.GetProtocol(),
	}
}

// ToProto converts a PortForwardSpec to a proto ExplicitPortForwardSpec.
func (s PortForwardSpec) ToProto() *graftv1.ExplicitPortForwardSpec {
	return &graftv1.ExplicitPortForwardSpec{
		RemotePort: s.RemotePort,
		LocalPort:  s.LocalPort,
		Protocol:   s.Protocol,
	}
}

// ParsePortSpecsToProto parses a slice of port spec strings into proto ExplicitPortForwardSpecs.
func ParsePortSpecsToProto(portSpecs []string) ([]*graftv1.ExplicitPortForwardSpec, error) {
	specs := make([]*graftv1.ExplicitPortForwardSpec, 0, len(portSpecs))

	for _, s := range portSpecs {
		parsed, err := ParsePortSpec(s)
		if err != nil {
			return nil, err
		}

		specs = append(specs, parsed.ToProto())
	}

	return specs, nil
}

// String returns the canonical string form of the port spec, suitable for round-tripping
// through ParsePortSpec.
func (s PortForwardSpec) String() string {
	var b strings.Builder

	if s.LocalPort != s.RemotePort {
		fmt.Fprintf(&b, "%d:%d", s.LocalPort, s.RemotePort)
	} else {
		fmt.Fprintf(&b, "%d", s.RemotePort)
	}

	if s.Protocol != ProtocolTCP {
		fmt.Fprintf(&b, "/%s", s.Protocol)
	}

	return b.String()
}
