package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/findings"
	"github.com/subganapathy/paved-road-agent/internal/proxy"
)

// fakeWrites records what the controller would post.
type fakeWrites struct {
	mu       sync.Mutex
	comments []proxy.CommentReq
	statuses []proxy.StatusReq
	commits  []proxy.CommitFileReq
}

func (f *fakeWrites) Comment(_ context.Context, r proxy.CommentReq) (any, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, r)
	return nil, 201, nil
}
func (f *fakeWrites) Status(_ context.Context, r proxy.StatusReq) (any, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, r)
	return nil, 201, nil
}
func (f *fakeWrites) CommitFile(_ context.Context, r proxy.CommitFileReq) (any, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, r)
	return map[string]any{"commit": "abc"}, 201, nil
}
func (f *fakeWrites) Comments(_ context.Context, _ string, _ int, _ int64) ([]proxy.PRComment, int, error) {
	return nil, 200, nil
}

func newController(t *testing.T, secret string) (*Controller, *fakeWrites) {
	w := &fakeWrites{}
	c, err := New(Config{Org: "acme", Watch: []string{"hello"}, WebhookSecret: secret, StateDir: t.TempDir(), CommitFiles: true}, anthropic.Client{}, w, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, w
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	c, _ := newController(t, "s3cret")
	body := []byte(`{"action":"opened","number":1,"pull_request":{"head":{"sha":"abc"}},"repository":{"full_name":"acme/hello"}}`)
	req := httptest.NewRequest("POST", "/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=nope")
	rec := httptest.NewRecorder()
	c.Webhook().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestWebhookIgnoresUnwatchedRepos(t *testing.T) {
	c, w := newController(t, "s3cret")
	body := []byte(`{"action":"opened","number":1,"pull_request":{"head":{"sha":"abc"}},"repository":{"full_name":"acme/other"}}`)
	req := httptest.NewRequest("POST", "/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign("s3cret", body))
	rec := httptest.NewRecorder()
	c.Webhook().ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	c.mu.Lock()
	n := len(c.runs)
	c.mu.Unlock()
	if n != 0 || len(w.statuses) != 0 {
		t.Fatalf("an unwatched repo must start nothing: runs=%d statuses=%d", n, len(w.statuses))
	}
}

// A comment on a PR with no open questions does nothing; one with open
// questions is remembered as seen even before the answer run.
func TestCommentBookkeeping(t *testing.T) {
	c, _ := newController(t, "")
	c.HandleComment(context.Background(), "acme/hello", 7, proxy.PRComment{ID: 10, Author: "alice", Body: "yes"})
	st := c.load("hello", 7)
	if st.LastCommentID != 10 {
		t.Fatalf("comment id not recorded: %+v", st)
	}
	// Our own comments and bots are ignored.
	c.HandleComment(context.Background(), "acme/hello", 7, proxy.PRComment{ID: 11, Author: "bot", Bot: true, Body: "yes"})
	c.HandleComment(context.Background(), "acme/hello", 7, proxy.PRComment{ID: 12, Author: "alice", Body: marker + " hi"})
	if st := c.load("hello", 7); st.LastCommentID != 10 {
		t.Fatalf("bot or marker comments must not advance the cursor: %+v", st)
	}
}

func TestGateFromVerdict(t *testing.T) {
	// The mapping from a report to a status is the gate; exercise it via
	// the same switch the review uses, with a report in hand.
	for _, tc := range []struct {
		verdict   findings.Severity
		questions int
		want      string
	}{
		{findings.Info, 0, "success"}, {findings.Warning, 0, "success"}, {findings.Blocking, 0, "failure"}, {findings.Blocking, 1, "pending"},
	} {
		state := gateState(tc.verdict, tc.questions)
		if state != tc.want {
			t.Errorf("verdict %s with %d questions: want %s, got %s", tc.verdict, tc.questions, tc.want, state)
		}
	}
}

var _ http.Handler = (*Controller)(nil).Webhook()
