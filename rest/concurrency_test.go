package rest_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// A mounted resource is shared by every request that reaches it: the binding,
// the model behind it and the hook registry are all built once and read from
// every goroutine serving one. This drives the whole path — parse, hooks,
// compile, scan, serialise, and for a write the transaction and the after-commit
// publication — from enough goroutines to matter, so that -race has something to
// look at. Nothing else in the suite runs two requests at the same time.
func TestConcurrentRequestsShareAMountedResource(t *testing.T) {
	db := newFakeDB(t, reply{
		cols: []string{"id", "org_id", "title", "body", "excerpt", "tags", "status", "view_count", "created_at"},
		rows: [][]any{
			{"p1", "o1", "One", "b", "e", []string{"x"}, "draft", int64(1), time.Now()},
		},
	})

	reg := sqlb.NewRegistry()
	sqlb.On[Post](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Post]) error {
		q.Where(sqlb.F("org_id").Eq("o1"))
		return nil
	})
	sqlb.On[Post](reg).BeforeCreate(func(_ context.Context, p *Post) error {
		p.OrgID = "o1"
		return nil
	})

	// A broker with a live subscriber, so writes fan out while reads are in
	// flight: publication happens on the after-commit callback of every write.
	broker := rest.NewBroker(rest.BrokerOptions{})
	defer broker.Close()
	if err := rest.PublishChanges[Post](reg, broker); err != nil {
		t.Fatal(err)
	}
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := broker.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range events {
		}
	}()

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Post, PostCreate, PostUpdate](api, sqlb.New(db).WithHooks(reg), rest.Options{
		Path:        "/posts",
		Ops:         rest.CRUD | rest.OpList,
		MaxPageSize: 50,
	}); err != nil {
		t.Fatal(err)
	}

	// The concurrent phase asserts nothing about bodies, so this is what says
	// the operations it hammers actually reach their handlers rather than
	// failing identically in parallel.
	for _, probe := range []struct {
		what string
		resp *httptest.ResponseRecorder
	}{
		{"list", api.Get("/posts?status=eq.draft&sort=-created_at&per_page=2")},
		{"read", api.Get("/posts/p1")},
		{"create", api.Post("/posts", map[string]any{"org_id": "o1", "title": "t", "body": "b"})},
		{"update", api.Patch("/posts/p1", map[string]any{"title": "patched"})},
	} {
		if probe.resp.Code >= 400 {
			t.Fatalf("%s: %d %s", probe.what, probe.resp.Code, probe.resp.Body)
		}
	}

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := range 20 {
				switch (i + j) % 5 {
				case 0:
					api.Get("/posts?status=eq.draft&sort=-created_at&per_page=2")
				case 1:
					api.Get("/posts?select=id,title&search=one&count=exact")
				case 2:
					api.Get("/posts/p1")
				case 3:
					api.Post("/posts", map[string]any{
						"org_id": "o1",
						"title":  fmt.Sprintf("t%d-%d", i, j),
						"body":   "b",
					})
				case 4:
					api.Patch("/posts/p1", map[string]any{"title": "patched"})
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

// The Broker is the one thing here that is concurrent by design rather than by
// deployment: publication, subscription and disconnection all reach the same
// state, and two of them close the same channel.
func TestConcurrentBrokerPublishAndSubscribe(t *testing.T) {
	// A buffer smaller than the fan-out rate, so subscribers really are dropped
	// mid-publish rather than the drop path going unexercised.
	b := rest.NewBroker(rest.BrokerOptions{History: 8, Buffer: 4})
	defer b.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.Publish(rest.Event{Table: "posts", Key: fmt.Sprint(i), Op: rest.Updated})
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithCancel(context.Background())
				ch, err := b.Subscribe(ctx, 1)
				if err != nil {
					cancel()
					return
				}
				select {
				case <-ch:
				case <-time.After(time.Millisecond):
				}
				cancel()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
