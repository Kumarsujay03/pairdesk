package screen

import "fmt"

// GstBinary is the launcher both ends execute.
const GstBinary = "gst-launch-1.0"

// Defaults for CaptureOptions.
const (
	DefaultFPS      = 15
	DefaultMaxWidth = 1280
	DefaultBitrate  = 1500 // kbps
	rtpPayloadType  = 96
	rtpMTU          = 1200
)

// CaptureOptions tunes the host-side encoder.
type CaptureOptions struct {
	FPS      int // frames per second, default 15
	MaxWidth int // scale down to at most this width keeping aspect, default 1280; 0 = no limit
	Bitrate  int // kbps, default 1500
	Logger   *slogLogger
}

func (o CaptureOptions) withDefaults() CaptureOptions {
	if o.FPS <= 0 {
		o.FPS = DefaultFPS
	}
	if o.MaxWidth == 0 {
		o.MaxWidth = DefaultMaxWidth
	}
	if o.MaxWidth < 0 {
		o.MaxWidth = 0
	}
	if o.Bitrate <= 0 {
		o.Bitrate = DefaultBitrate
	}
	return o
}

// encodeTail is the pipeline from raw video to RTP over UDP loopback, shared
// by the PipeWire and X11 sources.
func encodeTail(opt CaptureOptions, port int) []string {
	// The frame rate is fixed by videorate *after* capture rather than in
	// the caps the source negotiates: mutter's screencast streams advertise
	// a variable rate, and asking pipewiresrc for an exact 15/1 made the
	// server reject the format ("error set output format: -22").
	caps := fmt.Sprintf("video/x-raw,framerate=%d/1", opt.FPS)
	if opt.MaxWidth > 0 {
		// videoscale honours pixel-aspect-ratio, so fixing the width keeps
		// the aspect; height is left free. Width must be even for VP8.
		w := opt.MaxWidth &^ 1
		caps = fmt.Sprintf("video/x-raw,framerate=%d/1,width=%d,pixel-aspect-ratio=1/1", opt.FPS, w)
	}
	return []string{
		"!", "queue", "max-size-buffers=2", "leaky=downstream",
		"!", "videoconvert",
		"!", "videoscale",
		"!", "videorate", "drop-only=true",
		"!", caps,
		"!", "vp8enc",
		"deadline=1", "cpu-used=8",
		fmt.Sprintf("target-bitrate=%d", opt.Bitrate*1000),
		"keyframe-max-dist=30", "error-resilient=default",
		"!", "rtpvp8pay", fmt.Sprintf("pt=%d", rtpPayloadType), fmt.Sprintf("mtu=%d", rtpMTU),
		"!", "udpsink", "host=127.0.0.1", fmt.Sprintf("port=%d", port),
	}
}

// PipeWireCaptureArgs builds the gst-launch-1.0 argv for a portal screencast.
// childFD is the file descriptor number the PipeWire remote has inside the
// child (we pass it via ExtraFiles, so it is 3), node is the PipeWire node id
// returned by the portal's Start response.
func PipeWireCaptureArgs(opt CaptureOptions, childFD int, node uint32, port int) []string {
	opt = opt.withDefaults()
	args := []string{
		GstBinary, "-q",
		"pipewiresrc", fmt.Sprintf("fd=%d", childFD), fmt.Sprintf("path=%d", node), "do-timestamp=true",
	}
	return append(args, encodeTail(opt, port)...)
}

// X11CaptureArgs builds the gst-launch-1.0 argv for the ximagesrc fallback.
func X11CaptureArgs(opt CaptureOptions, port int) []string {
	opt = opt.withDefaults()
	args := []string{
		GstBinary, "-q",
		"ximagesrc", "use-damage=false", "do-timestamp=true",
	}
	return append(args, encodeTail(opt, port)...)
}

// ViewerArgs builds the gst-launch-1.0 argv that decodes RTP from UDP port
// and shows it in a window.
func ViewerArgs(port int) []string {
	return []string{
		GstBinary, "-q",
		"udpsrc", fmt.Sprintf("port=%d", port), "address=127.0.0.1",
		fmt.Sprintf("caps=application/x-rtp,media=video,encoding-name=VP8,payload=%d,clock-rate=90000", rtpPayloadType),
		"!", "rtpjitterbuffer", "latency=100",
		"!", "rtpvp8depay",
		"!", "vp8dec",
		"!", "videoconvert",
		"!", "autovideosink", "sync=false",
	}
}
