package orders_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

type notifyCapture struct {
	sent []string
}

func (c *notifyCapture) Send(_ context.Context, to, _ string) error {
	c.sent = append(c.sent, to)
	return nil
}

// TestNotifyJob_SkipsUserWithoutPhone proves the job resolves the phone at
// send time and quietly skips an account that has none.
func TestNotifyJob_SkipsUserWithoutPhone(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	emailOnly, err := users.New(pool).Resolve(ctx, auth.Identity{
		UID: "notify-email-only", Email: "notify-email-only@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	withPhone, err := users.New(pool).Resolve(ctx, auth.Identity{
		UID: "notify-with-phone", Phone: "+233241234567", Provider: "phone",
	})
	if err != nil {
		t.Fatal(err)
	}

	capture := &notifyCapture{}
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(capture, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	t.Cleanup(cancel)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	if _, err := client.Insert(ctx, notify.SMSArgs{
		UserID: emailOnly.ID, Template: notify.TemplateOrderAcceptedBuyer,
		Params: map[string]string{"orderIdShort": "a1b2c3d4"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	waitForNotifyJob(t, events)
	if len(capture.sent) != 0 {
		t.Errorf("sent = %v, want no message for a user without a phone", capture.sent)
	}

	if _, err := client.Insert(ctx, notify.SMSArgs{
		UserID: withPhone.ID, Template: notify.TemplateOrderAcceptedBuyer,
		Params: map[string]string{"orderIdShort": "a1b2c3d4"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	waitForNotifyJob(t, events)
	if len(capture.sent) != 1 || capture.sent[0] != "+233241234567" {
		t.Errorf("sent = %v, want the E.164 number resolved at send time", capture.sent)
	}
}

func waitForNotifyJob(t *testing.T, events <-chan *river.Event) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "notify.sms" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for notify.sms")
		}
	}
}
