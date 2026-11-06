package broker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestHTTPCodeAndConflict(t *testing.T) {
	for code, want := range map[int]bool{412: true, 409: true, 404: false, 403: false, 500: false} {
		err := fmt.Errorf("wrapped: %w", &googleapi.Error{Code: code})
		if httpCode(err) != code || isConflict(err) != want {
			t.Errorf("code %d: httpCode=%d conflict=%v", code, httpCode(err), isConflict(err))
		}
	}
	if httpCode(errors.New("x")) != 0 || isConflict(nil) {
		t.Error("plain errors are not API errors")
	}
}

func TestRetryConflicts(t *testing.T) {
	noSleep := func(context.Context, time.Duration) error { return nil }
	conflict := &googleapi.Error{Code: 412}

	n := 0
	err := retryConflicts(ctx, noSleep, func() error {
		if n++; n < 4 {
			return conflict
		}
		return nil
	})
	if err != nil || n != 4 {
		t.Errorf("err=%v attempts=%d", err, n)
	}

	n = 0
	if err := retryConflicts(ctx, noSleep, func() error { n++; return conflict }); err == nil || n != gcsConflictRetries {
		t.Errorf("err=%v attempts=%d", err, n)
	}

	n = 0
	boom := &googleapi.Error{Code: 403}
	if err := retryConflicts(ctx, noSleep, func() error { n++; return boom }); !errors.Is(err, boom) || n != 1 {
		t.Errorf("err=%v attempts=%d", err, n)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := retryConflicts(cctx, sleepCtx, func() error { return conflict }); !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v", err)
	}
}

func TestSplitBindingsKeepsConditionalAside(t *testing.T) {
	plain, cond := splitBindings(nil)
	if len(plain) != 0 || len(cond) != 0 {
		t.Error("empty input")
	}
}
