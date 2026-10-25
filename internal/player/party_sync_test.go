package player

import (
	"log/slog"
	"testing"
)

// B7-07：连续失败三次后判定 unaligned，且只广播一次。
func TestPartySyncGiveUpBroadcastsOnce(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()
	m := &Manager{deps: ManagerDeps{Logger: slog.Default(), Bus: bus}}
	st := newPartySyncState()
	m.partySync = map[string]*partySyncState{"p1": st}
	mem := st.member("g1")

	m.partySyncGiveUp("p1", "g1", mem, 4200)
	if !mem.unaligned {
		t.Fatal("应标记 unaligned")
	}
	// 再调两次：不能重复广播。
	m.partySyncGiveUp("p1", "g1", mem, 4300)
	m.partySyncGiveUp("p1", "g1", mem, 4400)

	n := 0
	for {
		select {
		case e := <-ch:
			if e.Type != EventGroupDrift {
				continue
			}
			n++
			if e.Unaligned == nil || !*e.Unaligned {
				t.Fatalf("事件应带 unaligned=true：%+v", e)
			}
			if e.Corrected == nil || *e.Corrected {
				t.Fatalf("放弃校正时 corrected 应为 false：%+v", e)
			}
		default:
			if n != 1 {
				t.Fatalf("unaligned 应只广播 1 次，实际 %d", n)
			}
			return
		}
	}
}

// 校正成功后 corrected:true 只发一次，随后回到静默。
func TestPartySyncConvergedBroadcastsOnce(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()
	m := &Manager{deps: ManagerDeps{Logger: slog.Default(), Bus: bus}}
	st := newPartySyncState()
	m.partySync = map[string]*partySyncState{"p1": st}
	mem := st.member("g1")
	mem.correcting = true

	m.partySyncConverged("p1", "g1", mem, 12)
	m.partySyncConverged("p1", "g1", mem, 30) // 已经收敛过，不再发

	n := 0
	for {
		select {
		case e := <-ch:
			if e.Type != EventGroupDrift {
				continue
			}
			n++
			if e.Corrected == nil || !*e.Corrected {
				t.Fatalf("收敛事件应 corrected=true：%+v", e)
			}
		default:
			if n != 1 {
				t.Fatalf("corrected 应只广播 1 次，实际 %d", n)
			}
			if mem.failures != 0 || mem.correcting {
				t.Fatalf("收敛后应清零状态：%+v", mem)
			}
			return
		}
	}
}
