package railway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckpointOutlivesNormalRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":{"sandboxCheckpointCreate":{"id":"cp","key":"base"}}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{Token: "test", EnvironmentID: "test", GraphQLEndpoint: server.URL,
		HTTPClient: &http.Client{Timeout: 10 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateCheckpoint(context.Background(), "sandbox", "base"); err != nil {
		t.Fatalf("checkpoint inherited short API timeout: %v", err)
	}
	if _, err := client.ListCheckpoints(context.Background()); err == nil {
		t.Fatal("ordinary requests lost their timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.CreateCheckpoint(ctx, "sandbox", "base"); err == nil {
		t.Fatal("checkpoint ignored caller deadline")
	}
}
