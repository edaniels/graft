//go:build linux

package graft

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edaniels/graft/errors"
)

// Get all descendant PIDs of a parent (recursive).
func getDescendants(parentPID int) (map[int]bool, error) {
	pids := map[int]bool{parentPID: true}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, errors.Wrap(err)
	}
	// Build parent->children map
	children := map[int][]int{}

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// Format: pid (comm) state ppid ...
		// Find closing paren to skip comm (which can contain spaces)
		s := string(stat)

		idx := strings.LastIndex(s, ")")
		if idx < 0 {
			continue
		}

		fields := strings.Fields(s[idx+1:])
		if len(fields) < 2 {
			continue
		}

		ppid, err := strconv.Atoi(fields[1]) // field 0=state, 1=ppid
		if err != nil {
			continue
		}

		children[ppid] = append(children[ppid], pid)
	}
	// BFS from parentPID
	queue := []int{parentPID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, child := range children[cur] {
			if !pids[child] {
				pids[child] = true
				queue = append(queue, child)
			}
		}
	}

	return pids, nil
}

// Get socket inodes for a PID.
func getSocketInodes(pid int) map[uint64]bool {
	inodes := map[uint64]bool{}
	fdPath := fmt.Sprintf("/proc/%d/fd", pid)

	entries, err := os.ReadDir(fdPath)
	if err != nil {
		return inodes // Process may have exited or we lack permission; skip.
	}

	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdPath, e.Name()))
		if err != nil {
			continue
		}
		// links look like "socket:[12345]"
		if strings.HasPrefix(link, "socket:[") {
			inode, err := strconv.ParseUint(link[8:len(link)-1], 10, 64)
			if err == nil {
				inodes[inode] = true
			}
		}
	}

	return inodes
}

// Parse /proc/net/tcp{,6} for LISTEN sockets and /proc/net/udp{,6} for bound UDP sockets.
func getListeningPorts() []ListeningPort {
	type procNetFile struct {
		path     string
		protocol string
		state    string // hex state to match
	}

	files := []procNetFile{
		{"/proc/net/tcp", "tcp", "0A"},  // TCP LISTEN
		{"/proc/net/tcp6", "tcp", "0A"}, // TCP6 LISTEN
		{"/proc/net/udp", "udp", "07"},  // UDP bound (TCP_CLOSE equivalent)
		{"/proc/net/udp6", "udp", "07"}, // UDP6 bound
	}

	var ports []ListeningPort

	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}

		ports = append(ports, parseProcNetEntries(string(data), f.protocol, f.state)...)
	}

	return ports
}

// GetPortsForParent returns all listening ports owned by the process tree rooted at parentPID.
func GetPortsForParent(parentPID int) ([]ListeningPort, error) {
	// 1. Get all descendant PIDs
	pids, err := getDescendants(parentPID)
	if err != nil {
		return nil, err
	}

	// 2. Collect socket inodes for all those PIDs
	inodeToPort := map[uint64]int{}

	for pid := range pids {
		inodes := getSocketInodes(pid)
		for inode := range inodes {
			inodeToPort[inode] = pid
		}
	}

	// 3. Get all listening ports and match by inode.
	// Deduplicate by (protocol, port) to avoid dual-stack sockets (which appear in both
	// /proc/net/tcp and /proc/net/tcp6 with the same inode) causing spurious change notifications.
	type dedupKey struct {
		Protocol string
		Port     int
	}

	listening := getListeningPorts()
	seen := map[dedupKey]struct{}{}

	var result []ListeningPort

	for _, lp := range listening {
		if pid, ok := inodeToPort[lp.Inode]; ok {
			dk := dedupKey{Protocol: lp.Protocol, Port: lp.Port}
			if _, dup := seen[dk]; dup {
				continue
			}

			seen[dk] = struct{}{}
			lp.PID = pid
			result = append(result, lp)
		}
	}

	// Sort for stable comparison. Include Host as tiebreaker for deterministic ordering.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Protocol != result[j].Protocol {
			return result[i].Protocol < result[j].Protocol
		}

		if result[i].Port != result[j].Port {
			return result[i].Port < result[j].Port
		}

		return result[i].Host < result[j].Host
	})

	return result, nil
}
