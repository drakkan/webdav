// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWalkToRoot(t *testing.T) {
	testCases := []struct {
		name string
		want []string
	}{{
		"/a/b/c/d",
		[]string{
			"/a/b/c/d",
			"/a/b/c",
			"/a/b",
			"/a",
			"/",
		},
	}, {
		"/a",
		[]string{
			"/a",
			"/",
		},
	}, {
		"/",
		[]string{
			"/",
		},
	}}

	for _, tc := range testCases {
		var got []string
		if !walkToRoot(tc.name, func(name0 string, first bool) bool {
			if first != (len(got) == 0) {
				t.Errorf("name=%q: first=%t but len(got)==%d", tc.name, first, len(got))
				return false
			}
			got = append(got, name0)
			return true
		}) {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("name=%q:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

var lockTestDurations = []time.Duration{
	infiniteTimeout, // infiniteTimeout means to never expire.
	0,               // A zero duration means to expire immediately.
	100 * time.Hour, // A very large duration will not expire in these tests.
}

// lockTestNames are the names of a set of mutually compatible locks. For each
// name fragment:
//   - _ means no explicit lock.
//   - i means an infinite-depth lock,
//   - z means a zero-depth lock,
var lockTestNames = []string{
	"/_/_/_/_/z",
	"/_/_/i",
	"/_/z",
	"/_/z/i",
	"/_/z/z",
	"/_/z/_/i",
	"/_/z/_/z",
	"/i",
	"/z",
	"/z/_/i",
	"/z/_/z",
}

func lockTestZeroDepth(name string) bool {
	switch name[len(name)-1] {
	case 'i':
		return false
	case 'z':
		return true
	}
	panic(fmt.Sprintf("lock name %q did not end with 'i' or 'z'", name))
}

func TestMemLSCanCreate(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)

	for _, name := range lockTestNames {
		_, err := m.Create(now, LockDetails{
			Root:      name,
			Duration:  infiniteTimeout,
			ZeroDepth: lockTestZeroDepth(name),
		})
		if err != nil {
			t.Fatalf("creating lock for %q: %v", name, err)
		}
	}

	wantCanCreate := func(name string, zeroDepth bool) bool {
		for _, n := range lockTestNames {
			switch {
			case n == name:
				// An existing lock has the same name as the proposed lock.
				return false
			case strings.HasPrefix(n, name):
				// An existing lock would be a child of the proposed lock,
				// which conflicts if the proposed lock has infinite depth.
				if !zeroDepth {
					return false
				}
			case strings.HasPrefix(name, n):
				// An existing lock would be an ancestor of the proposed lock,
				// which conflicts if the ancestor has infinite depth.
				if n[len(n)-1] == 'i' {
					return false
				}
			}
		}
		return true
	}

	var check func(int, string)
	check = func(recursion int, name string) {
		for _, zeroDepth := range []bool{false, true} {
			got := m.canCreate(name, zeroDepth)
			want := wantCanCreate(name, zeroDepth)
			if got != want {
				t.Errorf("canCreate name=%q zeroDepth=%t: got %t, want %t", name, zeroDepth, got, want)
			}
		}
		if recursion == 6 {
			return
		}
		if name != "/" {
			name += "/"
		}
		for _, c := range "_iz" {
			check(recursion+1, name+string(c))
		}
	}
	check(0, "/")
}

func TestMemLSLookup(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)

	badToken := m.nextToken()
	t.Logf("badToken=%q", badToken)

	for _, name := range lockTestNames {
		token, err := m.Create(now, LockDetails{
			Root:      name,
			Duration:  infiniteTimeout,
			ZeroDepth: lockTestZeroDepth(name),
		})
		if err != nil {
			t.Fatalf("creating lock for %q: %v", name, err)
		}
		t.Logf("%-15q -> node=%p token=%q", name, m.byName[name], token)
	}

	baseNames := append([]string{"/a", "/b/c"}, lockTestNames...)
	for _, baseName := range baseNames {
		for _, suffix := range []string{"", "/0", "/1/2/3"} {
			name := baseName + suffix

			goodToken := ""
			base := m.byName[baseName]
			if base != nil && (suffix == "" || !lockTestZeroDepth(baseName)) {
				goodToken = base.token
			}

			for _, token := range []string{badToken, goodToken} {
				if token == "" {
					continue
				}

				got := m.lookup(name, Condition{Token: token})
				want := base
				if token == badToken {
					want = nil
				}
				if got != want {
					t.Errorf("name=%-20qtoken=%q (bad=%t): got %p, want %p",
						name, token, token == badToken, got, want)
				}
			}
		}
	}
}

func TestMemLSConfirm(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)
	alice, err := m.Create(now, LockDetails{
		Root:      "/alice",
		Duration:  infiniteTimeout,
		ZeroDepth: false,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tweedle, err := m.Create(now, LockDetails{
		Root:      "/tweedle",
		Duration:  infiniteTimeout,
		ZeroDepth: false,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Create: inconsistent state: %v", err)
	}

	// Test a mismatch between name and condition.
	_, err = m.Confirm(now, "/tweedle/dee", "", Condition{Token: alice})
	if err != ErrConfirmationFailed {
		t.Fatalf("Confirm (mismatch): got %v, want ErrConfirmationFailed", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Confirm (mismatch): inconsistent state: %v", err)
	}

	// Test two names (that fall under the same lock) in the one Confirm call.
	release, err := m.Confirm(now, "/tweedle/dee", "/tweedle/dum", Condition{Token: tweedle})
	if err != nil {
		t.Fatalf("Confirm (twins): %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Confirm (twins): inconsistent state: %v", err)
	}
	release()
	if err := m.consistent(); err != nil {
		t.Fatalf("release (twins): inconsistent state: %v", err)
	}

	// Test the same two names in overlapping Confirm / release calls.
	releaseDee, err := m.Confirm(now, "/tweedle/dee", "", Condition{Token: tweedle})
	if err != nil {
		t.Fatalf("Confirm (sequence #0): %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Confirm (sequence #0): inconsistent state: %v", err)
	}

	_, err = m.Confirm(now, "/tweedle/dum", "", Condition{Token: tweedle})
	if err != ErrConfirmationFailed {
		t.Fatalf("Confirm (sequence #1): got %v, want ErrConfirmationFailed", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Confirm (sequence #1): inconsistent state: %v", err)
	}

	releaseDee()
	if err := m.consistent(); err != nil {
		t.Fatalf("release (sequence #2): inconsistent state: %v", err)
	}

	releaseDum, err := m.Confirm(now, "/tweedle/dum", "", Condition{Token: tweedle})
	if err != nil {
		t.Fatalf("Confirm (sequence #3): %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Confirm (sequence #3): inconsistent state: %v", err)
	}

	// Test that you can't unlock a held lock.
	err = m.Unlock(now, tweedle)
	if err != ErrLocked {
		t.Fatalf("Unlock (sequence #4): got %v, want ErrLocked", err)
	}

	releaseDum()
	if err := m.consistent(); err != nil {
		t.Fatalf("release (sequence #5): inconsistent state: %v", err)
	}

	err = m.Unlock(now, tweedle)
	if err != nil {
		t.Fatalf("Unlock (sequence #6): %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Unlock (sequence #6): inconsistent state: %v", err)
	}
}

func TestMemLSNonCanonicalRoot(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)
	token, err := m.Create(now, LockDetails{
		Root:     "/foo/./bar//",
		Duration: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Create: inconsistent state: %v", err)
	}
	if err := m.Unlock(now, token); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := m.consistent(); err != nil {
		t.Fatalf("Unlock: inconsistent state: %v", err)
	}
}

func TestMemLSExpiry(t *testing.T) {
	m := NewMemLS().(*memLS)
	testCases := []string{
		"setNow 0",
		"create /a.5",
		"want /a.5",
		"create /c.6",
		"want /a.5 /c.6",
		"create /a/b.7",
		"want /a.5 /a/b.7 /c.6",
		"setNow 4",
		"want /a.5 /a/b.7 /c.6",
		"setNow 5",
		"want /a/b.7 /c.6",
		"setNow 6",
		"want /a/b.7",
		"setNow 7",
		"want ",
		"setNow 8",
		"want ",
		"create /a.12",
		"create /b.13",
		"create /c.15",
		"create /a/d.16",
		"want /a.12 /a/d.16 /b.13 /c.15",
		"refresh /a.14",
		"want /a.14 /a/d.16 /b.13 /c.15",
		"setNow 12",
		"want /a.14 /a/d.16 /b.13 /c.15",
		"setNow 13",
		"want /a.14 /a/d.16 /c.15",
		"setNow 14",
		"want /a/d.16 /c.15",
		"refresh /a/d.20",
		"refresh /c.20",
		"want /a/d.20 /c.20",
		"setNow 20",
		"want ",
	}

	tokens := map[string]string{}
	zTime := time.Unix(0, 0)
	now := zTime
	for i, tc := range testCases {
		j := strings.IndexByte(tc, ' ')
		if j < 0 {
			t.Fatalf("test case #%d %q: invalid command", i, tc)
		}
		op, arg := tc[:j], tc[j+1:]
		switch op {
		default:
			t.Fatalf("test case #%d %q: invalid operation %q", i, tc, op)

		case "create", "refresh":
			parts := strings.Split(arg, ".")
			if len(parts) != 2 {
				t.Fatalf("test case #%d %q: invalid create", i, tc)
			}
			root := parts[0]
			d, err := strconv.Atoi(parts[1])
			if err != nil {
				t.Fatalf("test case #%d %q: invalid duration", i, tc)
			}
			dur := time.Unix(0, 0).Add(time.Duration(d) * time.Second).Sub(now)

			switch op {
			case "create":
				token, err := m.Create(now, LockDetails{
					Root:      root,
					Duration:  dur,
					ZeroDepth: true,
				})
				if err != nil {
					t.Fatalf("test case #%d %q: Create: %v", i, tc, err)
				}
				tokens[root] = token

			case "refresh":
				token := tokens[root]
				if token == "" {
					t.Fatalf("test case #%d %q: no token for %q", i, tc, root)
				}
				got, err := m.Refresh(now, token, dur)
				if err != nil {
					t.Fatalf("test case #%d %q: Refresh: %v", i, tc, err)
				}
				want := LockDetails{
					Root:      root,
					Duration:  dur,
					ZeroDepth: true,
				}
				if got != want {
					t.Fatalf("test case #%d %q:\ngot  %v\nwant %v", i, tc, got, want)
				}
			}

		case "setNow":
			d, err := strconv.Atoi(arg)
			if err != nil {
				t.Fatalf("test case #%d %q: invalid duration", i, tc)
			}
			now = time.Unix(0, 0).Add(time.Duration(d) * time.Second)

		case "want":
			m.mu.Lock()
			m.collectExpiredNodes(now)
			got := make([]string, 0, len(m.byToken))
			for _, n := range m.byToken {
				got = append(got, fmt.Sprintf("%s.%d",
					n.details.Root, n.expiry.Sub(zTime)/time.Second))
			}
			m.mu.Unlock()
			sort.Strings(got)
			want := []string{}
			if arg != "" {
				want = strings.Split(arg, " ")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("test case #%d %q:\ngot  %q\nwant %q", i, tc, got, want)
			}
		}

		if err := m.consistent(); err != nil {
			t.Fatalf("test case #%d %q: inconsistent state: %v", i, tc, err)
		}
	}
}

func TestMemLS(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)
	rng := rand.New(rand.NewSource(0))
	tokens := map[string]string{}
	nConfirm, nCreate, nRefresh, nUnlock := 0, 0, 0, 0
	const N = 2000

	for i := 0; i < N; i++ {
		name := lockTestNames[rng.Intn(len(lockTestNames))]
		duration := lockTestDurations[rng.Intn(len(lockTestDurations))]
		confirmed, unlocked := false, false

		// If the name was already locked, we randomly confirm/release, refresh
		// or unlock it. Otherwise, we create a lock.
		token := tokens[name]
		if token != "" {
			switch rng.Intn(3) {
			case 0:
				confirmed = true
				nConfirm++
				release, err := m.Confirm(now, name, "", Condition{Token: token})
				if err != nil {
					t.Fatalf("iteration #%d: Confirm %q: %v", i, name, err)
				}
				if err := m.consistent(); err != nil {
					t.Fatalf("iteration #%d: inconsistent state: %v", i, err)
				}
				release()

			case 1:
				nRefresh++
				if _, err := m.Refresh(now, token, duration); err != nil {
					t.Fatalf("iteration #%d: Refresh %q: %v", i, name, err)
				}

			case 2:
				unlocked = true
				nUnlock++
				if err := m.Unlock(now, token); err != nil {
					t.Fatalf("iteration #%d: Unlock %q: %v", i, name, err)
				}
			}

		} else {
			nCreate++
			var err error
			token, err = m.Create(now, LockDetails{
				Root:      name,
				Duration:  duration,
				ZeroDepth: lockTestZeroDepth(name),
			})
			if err != nil {
				t.Fatalf("iteration #%d: Create %q: %v", i, name, err)
			}
		}

		if !confirmed {
			if duration == 0 || unlocked {
				// A zero-duration lock should expire immediately and is
				// effectively equivalent to being unlocked.
				tokens[name] = ""
			} else {
				tokens[name] = token
			}
		}

		if err := m.consistent(); err != nil {
			t.Fatalf("iteration #%d: inconsistent state: %v", i, err)
		}
	}

	if nConfirm < N/10 {
		t.Fatalf("too few Confirm calls: got %d, want >= %d", nConfirm, N/10)
	}
	if nCreate < N/10 {
		t.Fatalf("too few Create calls: got %d, want >= %d", nCreate, N/10)
	}
	if nRefresh < N/10 {
		t.Fatalf("too few Refresh calls: got %d, want >= %d", nRefresh, N/10)
	}
	if nUnlock < N/10 {
		t.Fatalf("too few Unlock calls: got %d, want >= %d", nUnlock, N/10)
	}
}

func (m *memLS) consistent() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If m.byName is non-empty, then it must contain an entry for the root "/",
	// and its refCount should equal the number of locked nodes.
	if len(m.byName) > 0 {
		n := m.byName["/"]
		if n == nil {
			return fmt.Errorf(`non-empty m.byName does not contain the root "/"`)
		}
		if n.refCount != len(m.byToken) {
			return fmt.Errorf("root node refCount=%d, differs from len(m.byToken)=%d", n.refCount, len(m.byToken))
		}
	}

	for name, n := range m.byName {
		// The map keys should be consistent with the node's copy of the key.
		if n.details.Root != name {
			return fmt.Errorf("node name %q != byName map key %q", n.details.Root, name)
		}

		// A name must be clean, and start with a "/".
		if len(name) == 0 || name[0] != '/' {
			return fmt.Errorf(`node name %q does not start with "/"`, name)
		}
		if name != path.Clean(name) {
			return fmt.Errorf(`node name %q is not clean`, name)
		}

		// A node's refCount should be positive.
		if n.refCount <= 0 {
			return fmt.Errorf("non-positive refCount for node at name %q", name)
		}

		// A node's refCount should be the number of self-or-descendents that
		// are locked (i.e. have a non-empty token).
		var list []string
		for name0, n0 := range m.byName {
			// All of lockTestNames' name fragments are one byte long: '_', 'i' or 'z',
			// so strings.HasPrefix is equivalent to self-or-descendent name match.
			// We don't have to worry about "/foo/bar" being a false positive match
			// for "/foo/b".
			if strings.HasPrefix(name0, name) && n0.token != "" {
				list = append(list, name0)
			}
		}
		if n.refCount != len(list) {
			sort.Strings(list)
			return fmt.Errorf("node at name %q has refCount %d but locked self-or-descendents are %q (len=%d)",
				name, n.refCount, list, len(list))
		}

		// A node n is in m.byToken if it has a non-empty token.
		if n.token != "" {
			if _, ok := m.byToken[n.token]; !ok {
				return fmt.Errorf("node at name %q has token %q but not in m.byToken", name, n.token)
			}
		}

		// A node n is in m.byExpiry if it has a non-negative byExpiryIndex.
		if n.byExpiryIndex >= 0 {
			if n.byExpiryIndex >= len(m.byExpiry) {
				return fmt.Errorf("node at name %q has byExpiryIndex %d but m.byExpiry has length %d", name, n.byExpiryIndex, len(m.byExpiry))
			}
			if n != m.byExpiry[n.byExpiryIndex] {
				return fmt.Errorf("node at name %q has byExpiryIndex %d but that indexes a different node", name, n.byExpiryIndex)
			}
		}
	}

	for token, n := range m.byToken {
		// The map keys should be consistent with the node's copy of the key.
		if n.token != token {
			return fmt.Errorf("node token %q != byToken map key %q", n.token, token)
		}

		// Every node in m.byToken is in m.byName.
		if _, ok := m.byName[n.details.Root]; !ok {
			return fmt.Errorf("node at name %q in m.byToken but not in m.byName", n.details.Root)
		}
	}

	for i, n := range m.byExpiry {
		// The slice indices should be consistent with the node's copy of the index.
		if n.byExpiryIndex != i {
			return fmt.Errorf("node byExpiryIndex %d != byExpiry slice index %d", n.byExpiryIndex, i)
		}

		// Every node in m.byExpiry is in m.byName.
		if _, ok := m.byName[n.details.Root]; !ok {
			return fmt.Errorf("node at name %q in m.byExpiry but not in m.byName", n.details.Root)
		}

		// No node in m.byExpiry should be held.
		if n.held {
			return fmt.Errorf("node at name %q in m.byExpiry is held", n.details.Root)
		}
	}
	return nil
}

func TestParseTimeout(t *testing.T) {
	testCases := []struct {
		s       string
		want    time.Duration
		wantErr error
	}{{
		"",
		infiniteTimeout,
		nil,
	}, {
		"Infinite",
		infiniteTimeout,
		nil,
	}, {
		"Infinitesimal",
		0,
		errInvalidTimeout,
	}, {
		"infinite",
		0,
		errInvalidTimeout,
	}, {
		"Second-0",
		0 * time.Second,
		nil,
	}, {
		"Second-123",
		123 * time.Second,
		nil,
	}, {
		"  Second-456    ",
		456 * time.Second,
		nil,
	}, {
		"Second-4100000000",
		4100000000 * time.Second,
		nil,
	}, {
		"junk",
		0,
		errInvalidTimeout,
	}, {
		"Second-",
		0,
		errInvalidTimeout,
	}, {
		"Second--1",
		0,
		errInvalidTimeout,
	}, {
		"Second--123",
		0,
		errInvalidTimeout,
	}, {
		"Second-+123",
		0,
		errInvalidTimeout,
	}, {
		"Second-0x123",
		0,
		errInvalidTimeout,
	}, {
		"second-123",
		0,
		errInvalidTimeout,
	}, {
		"Second-4294967295",
		4294967295 * time.Second,
		nil,
	}, {
		// Section 10.7 says that "The timeout value for TimeType "Second"
		// must not be greater than 2^32-1."
		"Second-4294967296",
		0,
		errInvalidTimeout,
	}, {
		// This test case comes from section 9.10.9 of the spec. It says,
		//
		// "In this request, the client has specified that it desires an
		// infinite-length lock, if available, otherwise a timeout of 4.1
		// billion seconds, if available."
		//
		// The Go WebDAV package always supports infinite length locks,
		// and ignores the fallback after the comma.
		"Infinite, Second-4100000000",
		infiniteTimeout,
		nil,
	}}

	for _, tc := range testCases {
		got, gotErr := parseTimeout(tc.s)
		if got != tc.want || gotErr != tc.wantErr {
			t.Errorf("parsing %q:\ngot  %v, %v\nwant %v, %v", tc.s, got, gotErr, tc.want, tc.wantErr)
		}
	}
}

func TestMemLSMaxLocks(t *testing.T) {
	prev := maxLocksPerSystem
	maxLocksPerSystem = 4
	defer func() { maxLocksPerSystem = prev }()

	now := time.Unix(0, 0)
	m := NewMemLS()
	for i := 0; i < maxLocksPerSystem; i++ {
		_, err := m.Create(now, LockDetails{
			Root:      fmt.Sprintf("/file_%d", i),
			Duration:  time.Hour,
			ZeroDepth: true,
		})
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}
	_, err := m.Create(now, LockDetails{
		Root:      "/overflow",
		Duration:  time.Hour,
		ZeroDepth: true,
	})
	if err != ErrTooManyLocks {
		t.Fatalf("Create at cap: got %v, want %v", err, ErrTooManyLocks)
	}
}

func TestMemLSMaxLocksReclaimsExpired(t *testing.T) {
	prev := maxLocksPerSystem
	maxLocksPerSystem = 2
	defer func() { maxLocksPerSystem = prev }()

	now := time.Unix(0, 0)
	m := NewMemLS()
	for i := 0; i < maxLocksPerSystem; i++ {
		if _, err := m.Create(now, LockDetails{
			Root:      fmt.Sprintf("/short_%d", i),
			Duration:  time.Second,
			ZeroDepth: true,
		}); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}
	// After expiry, the cap check must let new locks in: collectExpiredNodes
	// runs before the cap test and frees the slots.
	later := now.Add(2 * time.Second)
	if _, err := m.Create(later, LockDetails{
		Root:      "/fresh",
		Duration:  time.Hour,
		ZeroDepth: true,
	}); err != nil {
		t.Fatalf("Create after expiry: got %v, want nil", err)
	}
}

const lockBody = `<?xml version="1.0" encoding="utf-8" ?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:exclusive/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
  <D:owner><D:href>http://example.org/owner</D:href></D:owner>
</D:lockinfo>`

func newLockTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(&Handler{
		FileSystem: NewMemFS(),
		LockSystem: NewMemLS(),
	})
}

func doLockRequest(t *testing.T, srv *httptest.Server, urlPath, body, timeout string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("LOCK", srv.URL+urlPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if timeout != "" {
		req.Header.Set("Timeout", timeout)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return res
}

func TestHandleLockClampsInfiniteTimeout(t *testing.T) {
	prev := maxLockDuration
	maxLockDuration = 17 * time.Second
	defer func() { maxLockDuration = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	res := doLockRequest(t, srv, "/file", lockBody, "Infinite")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected status: %d", res.StatusCode)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := fmt.Sprintf("<D:timeout>Second-%d</D:timeout>", int(maxLockDuration/time.Second))
	if !strings.Contains(string(got), want) {
		t.Fatalf("response missing clamped timeout %q in:\n%s", want, got)
	}
}

func TestHandleLockClampsExcessiveTimeout(t *testing.T) {
	prev := maxLockDuration
	maxLockDuration = 5 * time.Second
	defer func() { maxLockDuration = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	res := doLockRequest(t, srv, "/file", lockBody, "Second-99999")
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	want := fmt.Sprintf("<D:timeout>Second-%d</D:timeout>", int(maxLockDuration/time.Second))
	if !strings.Contains(string(got), want) {
		t.Fatalf("response missing clamped timeout %q in:\n%s", want, got)
	}
}

func TestHandleLockHonorsShortTimeout(t *testing.T) {
	srv := newLockTestServer(t)
	defer srv.Close()

	res := doLockRequest(t, srv, "/file", lockBody, "Second-30")
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(got), "<D:timeout>Second-30</D:timeout>") {
		t.Fatalf("requested-shorter-than-max timeout was not honored:\n%s", got)
	}
}

func TestHandleLockRefreshClampsTimeout(t *testing.T) {
	prev := maxLockDuration
	maxLockDuration = 9 * time.Second
	defer func() { maxLockDuration = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	res := doLockRequest(t, srv, "/file", lockBody, "Second-5")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("LOCK status: %d", res.StatusCode)
	}
	token := res.Header.Get("Lock-Token")
	if token == "" {
		t.Fatalf("missing Lock-Token in LOCK response")
	}

	req, err := http.NewRequest("LOCK", srv.URL+"/file", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Timeout", "Infinite")
	req.Header.Set("If", "("+token+")")
	refresh, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Refresh Do: %v", err)
	}
	defer refresh.Body.Close()
	got, _ := io.ReadAll(refresh.Body)
	want := fmt.Sprintf("<D:timeout>Second-%d</D:timeout>", int(maxLockDuration/time.Second))
	if !strings.Contains(string(got), want) {
		t.Fatalf("refresh response missing clamped timeout %q in:\n%s", want, got)
	}
}

func TestHandleLockBodyTooLarge(t *testing.T) {
	prev := maxLockBodySize
	maxLockBodySize = 256
	defer func() { maxLockBodySize = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	huge := `<?xml version="1.0" encoding="utf-8" ?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:exclusive/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
  <D:owner>` + strings.Repeat("A", int(maxLockBodySize)*4) + `</D:owner>
</D:lockinfo>`

	res := doLockRequest(t, srv, "/file", huge, "Second-30")
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want %d", res.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestHandleLockTooManyLocks(t *testing.T) {
	prevCap := maxLocksPerSystem
	maxLocksPerSystem = 2
	defer func() { maxLocksPerSystem = prevCap }()

	srv := newLockTestServer(t)
	defer srv.Close()

	for i := 0; i < maxLocksPerSystem; i++ {
		res := doLockRequest(t, srv, fmt.Sprintf("/file_%d", i), lockBody, "Second-3600")
		res.Body.Close()
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
			t.Fatalf("LOCK #%d: got status %d", i, res.StatusCode)
		}
	}
	res := doLockRequest(t, srv, "/file_overflow", lockBody, "Second-3600")
	defer res.Body.Close()
	if res.StatusCode != StatusInsufficientStorage {
		t.Fatalf("LOCK overflow: got status %d, want %d", res.StatusCode, StatusInsufficientStorage)
	}
}

func doMethodRequest(t *testing.T, srv *httptest.Server, method, urlPath, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+urlPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	if method == "PROPFIND" {
		req.Header.Set("Depth", "0")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return res
}

func TestHandlePropfindAcceptsValidBody(t *testing.T) {
	srv := newLockTestServer(t)
	defer srv.Close()

	body := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	res := doMethodRequest(t, srv, "PROPFIND", "/", body)
	defer res.Body.Close()
	if res.StatusCode != StatusMulti {
		t.Fatalf("PROPFIND: got status %d, want %d", res.StatusCode, StatusMulti)
	}
}

func TestHandlePropfindBodyTooLarge(t *testing.T) {
	prev := maxPropfindBodySize
	maxPropfindBodySize = 256
	defer func() { maxPropfindBodySize = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	huge := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:"><D:prop>` +
		strings.Repeat(`<D:foo/>`, int(maxPropfindBodySize)) +
		`</D:prop></D:propfind>`

	res := doMethodRequest(t, srv, "PROPFIND", "/", huge)
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PROPFIND: got status %d, want %d", res.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestHandleProppatchBodyTooLarge(t *testing.T) {
	prev := maxProppatchBodySize
	maxProppatchBodySize = 256
	defer func() { maxProppatchBodySize = prev }()

	srv := newLockTestServer(t)
	defer srv.Close()

	// Create the target file via PUT (PROPPATCH requires the resource to
	// exist and we don't want a lingering LOCK to make the response 423).
	put, err := http.NewRequest("PUT", srv.URL+"/file", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("NewRequest PUT: %v", err)
	}
	pres, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	pres.Body.Close()

	huge := `<?xml version="1.0" encoding="utf-8" ?>
<D:propertyupdate xmlns:D="DAV:" xmlns:Z="http://example.org/"><D:set><D:prop>
<Z:author>` + strings.Repeat("A", int(maxProppatchBodySize)*4) + `</Z:author>
</D:prop></D:set></D:propertyupdate>`

	res := doMethodRequest(t, srv, "PROPPATCH", "/file", huge)
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PROPPATCH: got status %d, want %d", res.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

// recursiveDirFS is a minimal FileSystem whose root and every subdirectory
// list a single child directory, producing an unbounded chain. It is used to
// exercise the walkFS recursion guard.
type recursiveDirFS struct{}

func (recursiveDirFS) Mkdir(context.Context, string, os.FileMode) error {
	return os.ErrPermission
}
func (recursiveDirFS) RemoveAll(context.Context, string) error { return os.ErrPermission }
func (recursiveDirFS) Rename(context.Context, string, string) error {
	return os.ErrPermission
}
func (recursiveDirFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	return recursiveDirFI{name: path.Base(name)}, nil
}
func (recursiveDirFS) OpenFile(_ context.Context, name string, _ int, _ os.FileMode) (File, error) {
	return &recursiveDirFile{name: name}, nil
}

type recursiveDirFI struct{ name string }

func (f recursiveDirFI) Name() string     { return f.name }
func (recursiveDirFI) Size() int64        { return 0 }
func (recursiveDirFI) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (recursiveDirFI) ModTime() time.Time { return time.Unix(0, 0) }
func (recursiveDirFI) IsDir() bool        { return true }
func (recursiveDirFI) Sys() interface{}   { return nil }

type recursiveDirFile struct {
	name string
	read bool
}

func (f *recursiveDirFile) Close() error { return nil }
func (f *recursiveDirFile) Read([]byte) (int, error) {
	return 0, io.EOF
}
func (f *recursiveDirFile) Seek(int64, int) (int64, error) { return 0, nil }
func (f *recursiveDirFile) Stat() (os.FileInfo, error) {
	return recursiveDirFI{name: path.Base(f.name)}, nil
}
func (f *recursiveDirFile) Write([]byte) (int, error) { return 0, os.ErrPermission }
func (f *recursiveDirFile) Readdir(int) ([]os.FileInfo, error) {
	if f.read {
		return nil, io.EOF
	}
	f.read = true
	return []os.FileInfo{recursiveDirFI{name: "child"}}, nil
}

func TestWalkFSRecursionGuard(t *testing.T) {
	prev := maxWalkRecursion
	maxWalkRecursion = 5
	defer func() { maxWalkRecursion = prev }()

	visited := 0
	walkFn := func(_ string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		visited++
		return nil
	}
	err := walkFS(context.Background(), recursiveDirFS{}, infiniteDepth, "/",
		recursiveDirFI{name: "/"}, walkFn)
	if err != errRecursionTooDeep {
		t.Fatalf("got err %v, want %v", err, errRecursionTooDeep)
	}
	if visited != maxWalkRecursion {
		t.Fatalf("visited %d nodes, want %d (the guard should fire on the next call)", visited, maxWalkRecursion)
	}
}
