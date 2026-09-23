package cli

import (
	"errors"
	"testing"
	"time"
)

func newFlashRetryForTest(t *testing.T) (flashRetry, *[]time.Duration, *[]string) {
	t.Helper()
	var sleeps []time.Duration
	var logs []string
	r := flashRetry{
		attempts: 3,
		wait:     time.Second,
		sleep:    func(d time.Duration) { sleeps = append(sleeps, d) },
		logf:     func(f string, a ...any) { logs = append(logs, f) },
	}
	return r, &sleeps, &logs
}

func TestFlashRetrySucceedsFirstAttempt(t *testing.T) {
	r, sleeps, _ := newFlashRetryForTest(t)
	calls := 0
	err := r.run(func() error { calls++; return nil })
	if err != nil {
		t.Fatalf("期望首次成功返回 nil，得到 %v", err)
	}
	if calls != 1 {
		t.Fatalf("期望调用 1 次，得到 %d", calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("成功时不应 sleep，得到 %v", *sleeps)
	}
}

func TestFlashRetrySucceedsOnRetry(t *testing.T) {
	r, sleeps, logs := newFlashRetryForTest(t)
	calls := 0
	err := r.run(func() error {
		calls++
		if calls < 3 {
			return errors.New("port busy")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("期望第 3 次成功返回 nil，得到 %v", err)
	}
	if calls != 3 {
		t.Fatalf("期望调用 3 次，得到 %d", calls)
	}
	if len(*sleeps) != 2 {
		t.Fatalf("期望 2 次 sleep，得到 %d", len(*sleeps))
	}
	if len(*logs) < 2 {
		t.Fatalf("期望失败与重试日志各 2 条，得到 %v", *logs)
	}
}

func TestFlashRetryExhaustsAttempts(t *testing.T) {
	r, sleeps, _ := newFlashRetryForTest(t)
	calls := 0
	giveUp := errors.New("device not functioning")
	err := r.run(func() error { calls++; return giveUp })
	if !errors.Is(err, giveUp) {
		t.Fatalf("期望返回最后一次错误 %v，得到 %v", giveUp, err)
	}
	if calls != 3 {
		t.Fatalf("期望调用 3 次，得到 %d", calls)
	}
	if len(*sleeps) != 2 {
		t.Fatalf("期望 2 次 sleep，得到 %d", len(*sleeps))
	}
}

func TestFlashRetryNoRetryErrorAborts(t *testing.T) {
	r, sleeps, _ := newFlashRetryForTest(t)
	calls := 0
	inner := errors.New("daemon unreachable")
	err := r.run(func() error { calls++; return noRetryError{inner} })
	if !errors.Is(err, inner) {
		t.Fatalf("期望解包返回 %v，得到 %v", inner, err)
	}
	if calls != 1 {
		t.Fatalf("noRetryError 不应重试，期望 1 次调用，得到 %d", calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("不应 sleep，得到 %v", *sleeps)
	}
}

func TestFlashRetrySingleAttempt(t *testing.T) {
	r, _, _ := newFlashRetryForTest(t)
	r.attempts = 1
	calls := 0
	err := r.run(func() error { calls++; return errors.New("x") })
	if err == nil {
		t.Fatal("期望错误透传")
	}
	if calls != 1 {
		t.Fatalf("attempts=1 期望单次调用，得到 %d", calls)
	}
}
