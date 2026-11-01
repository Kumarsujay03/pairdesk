package screen

import (
	"reflect"
	"testing"
)

func TestPipeWireCaptureArgs(t *testing.T) {
	got := PipeWireCaptureArgs(CaptureOptions{}, 3, 42, 5004)
	want := []string{
		"gst-launch-1.0", "-q",
		"pipewiresrc", "fd=3", "path=42", "do-timestamp=true",
		"!", "queue", "max-size-buffers=2", "leaky=downstream",
		"!", "videoconvert",
		"!", "videoscale",
		"!", "videorate", "drop-only=true",
		"!", "video/x-raw,framerate=15/1,width=1280,pixel-aspect-ratio=1/1",
		"!", "vp8enc", "deadline=1", "cpu-used=8", "target-bitrate=1500000", "keyframe-max-dist=30", "error-resilient=default",
		"!", "rtpvp8pay", "pt=96", "mtu=1200",
		"!", "udpsink", "host=127.0.0.1", "port=5004",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestX11CaptureArgsCustomOptions(t *testing.T) {
	got := X11CaptureArgs(CaptureOptions{FPS: 30, MaxWidth: -1, Bitrate: 800}, 6000)
	want := []string{
		"gst-launch-1.0", "-q",
		"ximagesrc", "use-damage=false", "do-timestamp=true",
		"!", "queue", "max-size-buffers=2", "leaky=downstream",
		"!", "videoconvert",
		"!", "videoscale",
		"!", "videorate", "drop-only=true",
		"!", "video/x-raw,framerate=30/1",
		"!", "vp8enc", "deadline=1", "cpu-used=8", "target-bitrate=800000", "keyframe-max-dist=30", "error-resilient=default",
		"!", "rtpvp8pay", "pt=96", "mtu=1200",
		"!", "udpsink", "host=127.0.0.1", "port=6000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// Odd widths are rounded down to even for VP8.
	got = X11CaptureArgs(CaptureOptions{MaxWidth: 1001}, 1)
	if got[17] != "video/x-raw,framerate=15/1,width=1000,pixel-aspect-ratio=1/1" {
		t.Errorf("odd width not rounded: %q", got[14])
	}
}

func TestViewerArgs(t *testing.T) {
	got := ViewerArgs(7000)
	want := []string{
		"gst-launch-1.0", "-q",
		"udpsrc", "port=7000", "address=127.0.0.1",
		"caps=application/x-rtp,media=video,encoding-name=VP8,payload=96,clock-rate=90000",
		"!", "rtpjitterbuffer", "latency=100",
		"!", "rtpvp8depay",
		"!", "vp8dec",
		"!", "videoconvert",
		"!", "autovideosink", "sync=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
