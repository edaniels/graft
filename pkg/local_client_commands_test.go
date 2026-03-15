package graft

import (
	"testing"

	"go.viam.com/test"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func TestSortConnectionNames(t *testing.T) {
	t.Run("current connection sorts first", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"alpha": {},
			"beta":  {Current: true},
			"gamma": {},
		}
		names := []string{"alpha", "beta", "gamma"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "beta")
		test.That(t, names[1], test.ShouldEqual, "alpha")
		test.That(t, names[2], test.ShouldEqual, "gamma")
	})

	t.Run("alphabetical when no current", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"gamma": {},
			"alpha": {},
			"beta":  {},
		}
		names := []string{"gamma", "alpha", "beta"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "alpha")
		test.That(t, names[1], test.ShouldEqual, "beta")
		test.That(t, names[2], test.ShouldEqual, "gamma")
	})

	t.Run("current first then alphabetical", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"zulu":  {Current: true},
			"alpha": {},
			"beta":  {},
		}
		names := []string{"zulu", "alpha", "beta"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "zulu")
		test.That(t, names[1], test.ShouldEqual, "alpha")
		test.That(t, names[2], test.ShouldEqual, "beta")
	})
}
