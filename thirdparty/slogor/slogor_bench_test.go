package slogor_test

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"gitlab.com/greyxor/slogor"
)

/**

go test -v -bench=.

-- Test for no color at each append

goos: linux
goarch: amd64
pkg: gitlab.com/greyxor/slogor
cpu: 11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz
BenchmarkSlogor-8   	  351196	      3547 ns/op
PASS
ok  	gitlab.com/greyxor/slogor	1.196s


-- No color as a lambda

goos: linux
goarch: amd64
pkg: gitlab.com/greyxor/slogor
cpu: 11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz
BenchmarkSlogor-8         304096	      3502 ns/op
PASS
ok  	gitlab.com/greyxor/slogor	1.107s

-- No color /dev/null redirect

Note: need to re-collect state for legacy master.

╭─  ~/repo/slogor   main@5b7d5e89 !1 ?1 ──────────────────────────────────────────────  1.22.8 ─╮
╰─  12:08:11 ❯ go test -bench=. -v                                                  took  2.307s ─╯
goos: linux
goarch: amd64
pkg: gitlab.com/greyxor/slogor
cpu: 11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz
BenchmarkSlogor
BenchmarkSlogor-8   	 1294120	       944.5 ns/op
PASS
ok  	gitlab.com/greyxor/slogor	2.164s

-- Legacy main

╭─  ~/repo/slogor  @2b00707a +1 ?2 🏷  v1.3.0 ──────────────────────────────────────────  1.22.8 ─╮
╰─  12:11:59 ❯ go test -bench=. -v                                                                ─╯
goos: linux
goarch: amd64
pkg: gitlab.com/greyxor/slogor
cpu: 11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz
BenchmarkSlogor
BenchmarkSlogor-8   	 1334641	       918.8 ns/op
PASS
ok  	gitlab.com/greyxor/slogor	2.140s

*/

func BenchmarkSlogor(b *testing.B) {
	slg := slog.New(
		slogor.NewHandler(os.NewFile(0, os.DevNull),
			slogor.SetTimeFormat(time.Kitchen),
			slogor.SetLevel(slog.LevelDebug),
		))

	for range b.N {
		slg.Error("emacs", slog.String("vim", "nano"))
	}
}
