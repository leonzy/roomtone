package player

import (
	"errors"
	"testing"
)

func TestBudgetAcquireRelease(t *testing.T) {
	b := NewBudget(2)
	rel1, err := b.Acquire("grp-1", "play")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := b.Acquire("grp-2", "play"); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if _, err := b.Acquire("grp-3", "play"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("超额应返回 ErrBudgetExhausted，得到 %v", err)
	}
	rel1()
	rel1() // 重复释放不应把计数打穿
	if b.Used() != 1 {
		t.Fatalf("Used = %d, want 1", b.Used())
	}
	if _, err := b.Acquire("grp-3", "play"); err != nil {
		t.Fatalf("释放后 Acquire 应成功: %v", err)
	}
}

func TestBudgetSameHolderIdempotent(t *testing.T) {
	b := NewBudget(4)
	if _, err := b.Acquire("grp-1", "play"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire("grp-1", "play"); err != nil {
		t.Fatalf("同一 holder 重复占用应复用配额: %v", err)
	}
	if b.Used() != 1 {
		t.Fatalf("Used = %d, want 1", b.Used())
	}
}

func TestBudgetSetMaxShrinksBelowUsed(t *testing.T) {
	b := NewBudget(4)
	for _, h := range []string{"a", "b", "c"} {
		if _, err := b.Acquire(h, "play"); err != nil {
			t.Fatal(err)
		}
	}
	b.SetMax(1)
	if _, err := b.Acquire("d", "play"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("缩容后新占用应失败，得到 %v", err)
	}
}
