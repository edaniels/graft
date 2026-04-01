package graft

import (
	"testing"

	"go.viam.com/test"
)

func TestIsPortSpec(t *testing.T) {
	// Valid port specs.
	for _, tc := range []string{
		"8080",
		"3000:8080",
		"5432/tcp",
		"5353/udp",
		"3000:8080/tcp",
		"3000:8080/udp",
		"1",
		"65535",
		"80:443",
	} {
		t.Run(tc, func(t *testing.T) {
			test.That(t, IsPortSpec(tc), test.ShouldBeTrue)
		})
	}

	// Not port specs (command names).
	for _, tc := range []string{
		"make",
		"python3",
		"go",
		"npm",
		"g++",
		"list",
		"remove",
		"which",
		"",
		"abc:def",
		"8080:abc",
		"abc:8080",
		"8080/ftp",
		"8080/",
		"/tcp",
	} {
		t.Run(tc, func(t *testing.T) {
			test.That(t, IsPortSpec(tc), test.ShouldBeFalse)
		})
	}
}

func TestParsePortSpec(t *testing.T) {
	t.Run("simple port", func(t *testing.T) {
		spec, err := ParsePortSpec("8080")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 8080)
		test.That(t, spec.LocalPort, test.ShouldEqual, 8080)
		test.That(t, spec.Protocol, test.ShouldEqual, "tcp")
	})

	t.Run("local:remote mapping", func(t *testing.T) {
		spec, err := ParsePortSpec("3000:8080")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 8080)
		test.That(t, spec.LocalPort, test.ShouldEqual, 3000)
		test.That(t, spec.Protocol, test.ShouldEqual, "tcp")
	})

	t.Run("explicit tcp", func(t *testing.T) {
		spec, err := ParsePortSpec("5432/tcp")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 5432)
		test.That(t, spec.LocalPort, test.ShouldEqual, 5432)
		test.That(t, spec.Protocol, test.ShouldEqual, "tcp")
	})

	t.Run("udp", func(t *testing.T) {
		spec, err := ParsePortSpec("5353/udp")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 5353)
		test.That(t, spec.LocalPort, test.ShouldEqual, 5353)
		test.That(t, spec.Protocol, test.ShouldEqual, "udp")
	})

	t.Run("full form", func(t *testing.T) {
		spec, err := ParsePortSpec("3000:8080/udp")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 8080)
		test.That(t, spec.LocalPort, test.ShouldEqual, 3000)
		test.That(t, spec.Protocol, test.ShouldEqual, "udp")
	})

	t.Run("port 1", func(t *testing.T) {
		spec, err := ParsePortSpec("1")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 1)
		test.That(t, spec.LocalPort, test.ShouldEqual, 1)
	})

	t.Run("port 65535", func(t *testing.T) {
		spec, err := ParsePortSpec("65535")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, spec.RemotePort, test.ShouldEqual, 65535)
	})

	// Invalid specs.
	t.Run("port 0", func(t *testing.T) {
		_, err := ParsePortSpec("0")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("port above 65535", func(t *testing.T) {
		_, err := ParsePortSpec("65536")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("not a port spec", func(t *testing.T) {
		_, err := ParsePortSpec("make")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("invalid protocol", func(t *testing.T) {
		_, err := ParsePortSpec("8080/ftp")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("empty string", func(t *testing.T) {
		_, err := ParsePortSpec("")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("local port 0", func(t *testing.T) {
		_, err := ParsePortSpec("0:8080")
		test.That(t, err, test.ShouldNotBeNil)
	})

	t.Run("remote port 0", func(t *testing.T) {
		_, err := ParsePortSpec("3000:0")
		test.That(t, err, test.ShouldNotBeNil)
	})
}

func TestPortForwardSpecString(t *testing.T) {
	t.Run("same local and remote tcp", func(t *testing.T) {
		spec := PortForwardSpec{RemotePort: 8080, LocalPort: 8080, Protocol: "tcp"}
		test.That(t, spec.String(), test.ShouldEqual, "8080")
	})

	t.Run("different local and remote tcp", func(t *testing.T) {
		spec := PortForwardSpec{RemotePort: 8080, LocalPort: 3000, Protocol: "tcp"}
		test.That(t, spec.String(), test.ShouldEqual, "3000:8080")
	})

	t.Run("same local and remote udp", func(t *testing.T) {
		spec := PortForwardSpec{RemotePort: 5353, LocalPort: 5353, Protocol: "udp"}
		test.That(t, spec.String(), test.ShouldEqual, "5353/udp")
	})

	t.Run("different local and remote udp", func(t *testing.T) {
		spec := PortForwardSpec{RemotePort: 8080, LocalPort: 3000, Protocol: "udp"}
		test.That(t, spec.String(), test.ShouldEqual, "3000:8080/udp")
	})
}

func TestParsePortSpecRoundTrip(t *testing.T) {
	for _, input := range []string{
		"8080",
		"3000:8080",
		"5353/udp",
		"3000:8080/udp",
	} {
		t.Run(input, func(t *testing.T) {
			spec, err := ParsePortSpec(input)
			test.That(t, err, test.ShouldBeNil)
			test.That(t, spec.String(), test.ShouldEqual, input)
		})
	}
}
