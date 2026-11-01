package pipeline

import (
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
	"testing"
	"time"
)

func TestMemberDelayChangesActualPCMNotOnlyStartTimestamp(t *testing.T) {
	output := fake.New("study")
	w := newSinkWriter(sink.Target{PlayerID: "study", Sink: output, Delay: 60 * time.Millisecond}, 10, 44100, 882, nil, nil)
	go w.run()
	at := time.Now()
	data := make([]byte, 882*4)
	for i := range data {
		data[i] = 1
	}
	w.push(sink.Frame{SampleIndex: 0, PlayAt: at, Data: data})
	close(w.ch)
	<-w.done
	frames := output.Frames()
	if len(frames) != 4 {
		t.Fatalf("60ms must insert 3 silence frames, got %d total frames", len(frames))
	}
	for i := 0; i < 3; i++ {
		if !frames[i].Silent {
			t.Fatal("delay prefix contains music")
		}
	}
	if frames[3].Silent || frames[3].SampleIndex != 2646 || frames[3].PlayAt.Sub(at) != 60*time.Millisecond {
		t.Fatalf("music was not delayed: %+v", frames[3])
	}
}

func TestNegativeMemberDelayCompressesTimestampAfterSkippedPCM(t *testing.T) {
	output := fake.New("ahead")
	w := newSinkWriter(sink.Target{PlayerID: "ahead", Sink: output, Delay: -40 * time.Millisecond}, 10, 44100, 882, nil, nil)
	go w.run()
	at := time.Now()
	data := make([]byte, 882*4)
	for i := range data {
		data[i] = 1
	}
	for i := 0; i < 3; i++ {
		w.push(sink.Frame{SampleIndex: int64(i * 882), PlayAt: at.Add(time.Duration(i) * 20 * time.Millisecond), Data: data})
	}
	close(w.ch)
	<-w.done
	frames := output.Frames()
	if len(frames) != 1 || frames[0].SampleIndex != 0 || !frames[0].PlayAt.Equal(at) {
		t.Fatalf("negative delay left timestamp holes: %+v", frames)
	}
}

func TestGaplessMemberDelayKeepsOffsetWithoutRepeatingSilence(t *testing.T) {
	output := fake.New("study")
	target := sink.Target{PlayerID: "study", Sink: output, Delay: 60 * time.Millisecond}
	at := time.Now()
	data := make([]byte, 882*4)
	for i := range data {
		data[i] = 1
	}
	first := newFanout(10, 44100, 882, nil, nil)
	first.reconcile([]sink.Target{target}, 1)
	first.push(sink.Frame{SampleIndex: 0, PlayAt: at, Data: data})
	first.close()
	second := newFanout(10, 44100, 882, nil, nil)
	second.initialDelays = first.finalDelays
	second.reconcile([]sink.Target{target}, 1)
	second.push(sink.Frame{SampleIndex: 882, PlayAt: at.Add(20 * time.Millisecond), Data: data})
	second.close()
	frames := output.Frames()
	if len(frames) != 5 || frames[4].Silent || frames[4].SampleIndex != 3528 || frames[4].PlayAt.Sub(at) != 80*time.Millisecond {
		t.Fatalf("gapless delay repeated or timestamps jumped: %+v", frames)
	}
}
