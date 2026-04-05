package graft

import (
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

func TestCollectCommandsFromPATH(t *testing.T) {
	tempDirGlobal := t.TempDir()
	tempDirByDir1 := t.TempDir()
	tempDirByDir2 := t.TempDir()

	exec1, err := os.Create(filepath.Join(tempDirGlobal, "exec1"))
	test.That(t, err, test.ShouldBeNil)

	exec2, err := os.Create(filepath.Join(tempDirGlobal, "exec2"))
	test.That(t, err, test.ShouldBeNil)

	exec3, err := os.Create(filepath.Join(tempDirByDir1, "exec3"))
	test.That(t, err, test.ShouldBeNil)
	exec4, err := os.Create(filepath.Join(tempDirByDir1, "exec4"))
	test.That(t, err, test.ShouldBeNil)

	exec5, err := os.Create(filepath.Join(tempDirByDir2, "exec5"))
	test.That(t, err, test.ShouldBeNil)
	exec6, err := os.Create(filepath.Join(tempDirByDir2, "exec6"))
	test.That(t, err, test.ShouldBeNil)

	newPath := tempDirGlobal

	curPath := os.Getenv("PATH")
	if curPath != "" {
		newPath = newPath + ":" + curPath
	}

	t.Setenv("PATH", newPath)

	collected, byDir := collectCommandsFromPATH(nil)
	test.That(t, collected, test.ShouldNotContain, exec1.Name())
	test.That(t, collected, test.ShouldNotContain, exec2.Name())
	test.That(t, byDir, test.ShouldBeEmpty)

	test.That(t, exec1.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec2.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec3.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec4.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec5.Chmod(0o777), test.ShouldBeNil)
	test.That(t, exec6.Chmod(0o777), test.ShouldBeNil)

	collected, byDir = collectCommandsFromPATH(map[string][]string{
		"prov1": {tempDirByDir1, tempDirByDir2},
		"prov2": {tempDirByDir2},
	})
	test.That(t, collected, test.ShouldContain, exec1.Name())
	test.That(t, collected, test.ShouldContain, exec2.Name())
	test.That(t, byDir, test.ShouldResemble, map[string][]string{
		"prov1": {exec3.Name(), exec4.Name(), exec5.Name(), exec6.Name()},
		"prov2": {exec5.Name(), exec6.Name()},
	})
}
