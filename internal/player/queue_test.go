package player

import "testing"

func TestQueueNavigation(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 2, 3}, 0)
	if got, _ := q.CurrentSong(); got != 1 {
		t.Fatalf("current = %d, want 1", got)
	}
	if ok := q.Next(false); !ok {
		t.Fatal("Next 应成功")
	}
	if got, _ := q.CurrentSong(); got != 2 {
		t.Fatalf("current = %d, want 2", got)
	}
	q.Prev()
	if got, _ := q.CurrentSong(); got != 1 {
		t.Fatalf("current = %d, want 1", got)
	}
}

func TestQueueRepeatOne(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{7, 8}, 0)
	q.SetRepeat(RepeatOne)
	if ok := q.Next(true); !ok {
		t.Fatal("repeat one 下自然播完应原地重播")
	}
	if got, _ := q.CurrentSong(); got != 7 {
		t.Fatalf("current = %d, want 7", got)
	}
}

func TestQueueEndOfList(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 2}, 1)
	if ok := q.Next(false); ok {
		t.Fatal("列表末尾且未开 repeat 时 Next 应返回 false")
	}
	q.SetRepeat(RepeatAll)
	if ok := q.Next(false); !ok {
		t.Fatal("repeat all 下应回到开头")
	}
	if got, _ := q.CurrentSong(); got != 1 {
		t.Fatalf("current = %d, want 1", got)
	}
}

func TestQueueInsertNextAndRemove(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 2, 3}, 0)
	q.InsertNext(9)
	if items := q.Items(); len(items) != 4 || items[1] != 9 {
		t.Fatalf("insert next 后 items = %v", items)
	}
	if err := q.Remove(1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if items := q.Items(); len(items) != 3 {
		t.Fatalf("remove 后 items = %v", items)
	}
}

func TestQueueShuffleKeepsCurrent(t *testing.T) {
	q := NewQueue()
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	q.Replace(ids, 10)
	q.SetShuffle(true)
	cur, _ := q.CurrentSong()
	if cur != 11 {
		t.Fatalf("开 shuffle 后当前曲目应保持 11，得到 %d", cur)
	}
	seen := map[int64]bool{}
	for i := 0; i < 50; i++ {
		id, err := q.CurrentSong()
		if err != nil {
			t.Fatalf("CurrentSong: %v", err)
		}
		seen[id] = true
		if !q.Next(true) {
			break
		}
	}
	if len(seen) != 50 {
		t.Fatalf("shuffle 一轮应覆盖全部曲目，实际 %d", len(seen))
	}
}

func TestQueueSnapshotRestore(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 2, 3}, 2)
	q.SetRepeat(RepeatAll)
	q.SetShuffle(true)
	snap := q.Snapshot()

	other := NewQueue()
	other.Restore(snap)
	if got, _ := other.CurrentSong(); got != 3 {
		t.Fatalf("restore 后 current = %d, want 3", got)
	}
	if other.Repeat() != RepeatAll || !other.Shuffle() {
		t.Fatal("restore 后 repeat/shuffle 未恢复")
	}
}
