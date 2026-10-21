package updater

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/google/go-github/v32/github"
	"github.com/hashicorp/go-version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGithub serves the release endpoints of one repository and records every
// request path
type fakeGithub struct {
	mu       sync.Mutex
	paths    []string
	releases []string // tags, newest first
}

func (f *fakeGithub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()

	base := fmt.Sprintf("/repos/%s/%s/releases", owner, repository)

	switch r.URL.Path {
	case base + "/latest":
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": f.releases[0]})
	case base:
		var res []map[string]any
		for _, tag := range f.releases {
			res = append(res, map[string]any{"tag_name": tag, "body": "notes " + tag})
		}
		_ = json.NewEncoder(w).Encode(res)
	case "/markdown":
		var req struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprintf(w, "<p>%s</p>", req.Text)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGithub) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func newFakeWatch(t *testing.T, releases ...string) (*watch, *fakeGithub, chan util.Param) {
	t.Helper()

	fake := &fakeGithub{releases: releases}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.Client())
	u, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	client.BaseURL = u

	out := make(chan util.Param, 4)

	return &watch{
		log:     util.NewLogger("test"),
		outChan: out,
		repo:    &Repo{owner: owner, repository: repository, Client: client},
	}, fake, out
}

func TestForkRepository(t *testing.T) {
	assert.Equal(t, "jeffreyvm", owner)
	assert.Equal(t, "evcc", repository)

	// semver orders a fork release before the plain upstream release with the same
	// base, so the check must only ever compare against releases of the fork
	assert.True(t, version.Must(version.NewVersion("0.317.2")).GreaterThan(version.Must(version.NewVersion("0.317.2-ems.1"))))
}

func TestFindReleaseUpdateFork(t *testing.T) {
	for _, tc := range []struct {
		installed, latest string
		update            bool
	}{
		{"0.317.2-ems.1", "0.317.2-ems.2", true},
		{"0.317.2-ems.5", "0.317.3-ems.1", true},
		{"0.317.2-ems.9", "0.317.2-ems.10", true},
		{"0.317.2-ems.2", "0.317.2-ems.2", false},
		{"0.317.2-ems.2", "0.317.2-ems.1", false},
		{"0.317.3-ems.1", "0.317.2-ems.5", false},
		// the upstream package is semver-newer than the first fork release: the
		// switch to the fork is a deliberate downgrade, not an announced update
		{"0.317.2", "0.317.2-ems.1", false},
	} {
		t.Run(tc.installed+"→"+tc.latest, func(t *testing.T) {
			w, fake, out := newFakeWatch(t, tc.latest, tc.installed)

			rel, err := w.findReleaseUpdate(tc.installed)
			require.NoError(t, err)

			if !tc.update {
				assert.Nil(t, rel)
			} else {
				require.NotNil(t, rel)
				assert.Equal(t, tc.latest, rel.GetTagName())

				// release notes are fetched in the background, newer releases only
				select {
				case p := <-out:
					assert.Equal(t, "releaseNotes", p.Key)
					notes, _ := p.Val.(string)
					assert.Contains(t, notes, "notes "+tc.latest)
					assert.NotContains(t, notes, "notes "+tc.installed)
				case <-time.After(5 * time.Second):
					t.Fatal("no release notes")
				}
			}

			for _, p := range fake.requested() {
				assert.True(t, p == "/markdown" || strings.HasPrefix(p, "/repos/jeffreyvm/evcc/"), "unexpected request %s", p)
			}
		})
	}
}
