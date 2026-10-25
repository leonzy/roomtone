package player

import (
	"testing"
	"time"
)

func TestEventBusSeqMonotonic(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()

	a := bus.Publish(Event{Type: EventState})
	b := bus.Publish(Event{Type: EventTargetChanged})
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("seq 应连续递增，得到 %d %d", a.Seq, b.Seq)
	}
	if bus.CurrentSeq() != 2 {
		t.Fatalf("CurrentSeq = %d", bus.CurrentSeq())
	}
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			if e.Seq != uint64(i+1) {
				t.Fatalf("订阅者收到乱序 seq: %d", e.Seq)
			}
		case <-time.After(time.Second):
			t.Fatal("订阅者未收到事件")
		}
	}
}

func TestEventBusSlowSubscriberDrops(t *testing.T) {
	bus := NewEventBus()
	_, cancel := bus.Subscribe() // 故意不读，填满 64 条缓冲
	defer cancel()

	for i := 0; i < 100; i++ {
		bus.Publish(Event{Type: EventEQChanged})
	}
	if bus.Dropped() != 36 {
		t.Fatalf("Dropped = %d, want 36", bus.Dropped())
	}
	if bus.CurrentSeq() != 100 {
		t.Fatalf("丢弃不应影响 seq，CurrentSeq = %d", bus.CurrentSeq())
	}
}

func TestEventBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe()
	cancel()
	bus.Publish(Event{Type: EventState})
	if _, ok := <-ch; ok {
		t.Fatal("取消订阅后通道应关闭")
	}
	// 再 publish 不能 panic（subs 已删除）
	bus.Publish(Event{Type: EventState})
}

func TestEventJSONRoundTrip(t *testing.T) {
	e := Event{Type: EventSpeakerSeen, PlayerID: "p1", Seq: 9}
	data, err := e.EventJSON()
	if err != nil {
		t.Fatalf("EventJSON: %v", err)
	}
	got, err := DecodeEvent(data)
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if got.Seq != 9 || got.PlayerID != "p1" || got.Type != EventSpeakerSeen {
		t.Fatalf("round trip 丢失字段: %+v", got)
	}
}

func TestSeqFilter(t *testing.T) {
	var f SeqFilter
	if !f.Accept(5) {
		t.Fatal("首次 seq 应接受")
	}
	if f.Accept(5) || f.Accept(4) {
		t.Fatal("重复或更旧的 seq 应拒绝")
	}
	if !f.Accept(6) {
		t.Fatal("更新的 seq 应接受")
	}
	if f.Last() != 6 {
		t.Fatalf("Last = %d", f.Last())
	}
}
