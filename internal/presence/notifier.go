package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Notifier is the small contract the WebSocket gateway depends on to feed the
// Presence Service connection lifecycle. Keeping it an interface (rather than
// importing the concrete client) means the WS hub couples to three method
// signatures, not to presence internals — and a nil Notifier is a valid no-op,
// so the gateway still runs standalone without a Presence Service.
type Notifier interface {
	Connect(ctx context.Context, playerID string) error
	Disconnect(ctx context.Context, playerID string) error
	Heartbeat(ctx context.Context, playerID string) error
}

// HTTPNotifier calls the Presence Service over HTTP. It propagates the W3C trace
// context on every call so a WS connect/disconnect shows up in the same trace as
// the resulting presence transition.
type HTTPNotifier struct {
	baseURL string
	client  *http.Client
}

// NewHTTPNotifier builds a notifier targeting the Presence Service base URL
// (e.g. http://presence:8086). The timeout keeps a slow/absent presence service
// from blocking WS connection handling.
func NewHTTPNotifier(baseURL string, timeout time.Duration) *HTTPNotifier {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPNotifier{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
	}
}

func (n *HTTPNotifier) Connect(ctx context.Context, playerID string) error {
	return n.post(ctx, "/presence/connect", playerID)
}

func (n *HTTPNotifier) Disconnect(ctx context.Context, playerID string) error {
	return n.post(ctx, "/presence/disconnect", playerID)
}

func (n *HTTPNotifier) Heartbeat(ctx context.Context, playerID string) error {
	return n.post(ctx, "/presence/heartbeat", playerID)
}

func (n *HTTPNotifier) post(ctx context.Context, path, playerID string) error {
	body, err := json.Marshal(playerRequest{PlayerID: playerID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Propagate trace context so the WS->presence hop joins the same trace.
	carrier := tracing.InjectCarrier(ctx, map[string]string{})
	for k, v := range carrier {
		req.Header.Set(k, v)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("presence %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// NoopNotifier satisfies Notifier and does nothing. It is the default when no
// Presence Service is configured, so the WS gateway runs unchanged.
type NoopNotifier struct{}

func (NoopNotifier) Connect(context.Context, string) error    { return nil }
func (NoopNotifier) Disconnect(context.Context, string) error { return nil }
func (NoopNotifier) Heartbeat(context.Context, string) error  { return nil }
