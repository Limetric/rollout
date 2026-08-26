package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// Play rejects a store image for its dimensions with a message that names
// neither the file nor the constraint, after the whole upload has already been
// spent. Reading the header locally costs a few bytes and turns that into a
// sentence the user can act on before anything is staged.

// imageMeta is what a preview needs to say about a local image.
type imageMeta struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	// Warnings are Play's published constraints this file does not meet. They
	// are warnings rather than errors: the rules move, they differ by device
	// type, and refusing an upload Play would have accepted is worse than
	// letting the API have the final word.
	Warnings []string `json:"warnings,omitempty"`
}

// readImageMeta decodes an image's header and checks it against the constraints
// for the image type it is destined for.
func readImageMeta(path, imageType string) (*imageMeta, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory, not an image", path)
	}
	header, err := readFileHead(path, 1024)
	if err != nil {
		return nil, err
	}
	format, width, height, err := decodeImageHeader(header)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", path, err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return nil, err
	}

	meta := &imageMeta{Path: path, Format: format, Width: width, Height: height, Bytes: info.Size(), SHA256: sum}
	meta.Warnings = imageWarnings(imageType, meta)
	return meta, nil
}

func readFileHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	return buf[:read], nil
}

var (
	pngMagic  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	jpegMagic = []byte{0xff, 0xd8}
)

// decodeImageHeader reads dimensions out of a PNG or JPEG header. Play accepts
// only these two for store images, so anything else is worth naming as such
// rather than passing through to a generic API rejection.
func decodeImageHeader(data []byte) (format string, width, height int, err error) {
	switch {
	case len(data) >= 24 && string(data[:8]) == string(pngMagic):
		// IHDR is always the first chunk, and its width and height are the two
		// big-endian uint32s at offset 16.
		return "png", int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24])), nil
	case len(data) >= 2 && data[0] == jpegMagic[0] && data[1] == jpegMagic[1]:
		w, h, err := decodeJPEGSize(data)
		if err != nil {
			return "", 0, 0, err
		}
		return "jpeg", w, h, nil
	default:
		return "", 0, 0, fmt.Errorf("not a PNG or JPEG — Play takes only those two for store images")
	}
}

// decodeJPEGSize walks the JPEG marker segments to the start-of-frame, which is
// the only place the dimensions live.
func decodeJPEGSize(data []byte) (width, height int, err error) {
	for i := 2; i+9 < len(data); {
		if data[i] != 0xff {
			i++
			continue
		}
		marker := data[i+1]
		// Standalone markers carry no length.
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			i += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		// SOF0–SOF15, excluding the DHT/JPG/DAC markers interleaved among them.
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			return int(binary.BigEndian.Uint16(data[i+7 : i+9])), int(binary.BigEndian.Uint16(data[i+5 : i+7])), nil
		}
		i += 2 + length
	}
	// The header we read is bounded, so a JPEG with a lot of metadata before
	// its frame is not malformed — we just cannot see that far.
	return 0, 0, fmt.Errorf("could not find the JPEG frame header in the first bytes of the file")
}

// Play's published store-image constraints, as of the v3 documentation.
const (
	minImageDimension = 320
	maxImageDimension = 3840
	maxScreenshots    = 8
)

// imageWarnings reports where a file misses Play's constraints for its type.
func imageWarnings(imageType string, meta *imageMeta) []string {
	var warnings []string
	switch imageType {
	case "icon":
		if meta.Width != 512 || meta.Height != 512 {
			warnings = append(warnings, fmt.Sprintf("the app icon must be 512×512; this is %d×%d", meta.Width, meta.Height))
		}
		if meta.Format != "png" {
			warnings = append(warnings, "the app icon must be a 32-bit PNG")
		}
	case "featureGraphic":
		if meta.Width != 1024 || meta.Height != 500 {
			warnings = append(warnings, fmt.Sprintf("the feature graphic must be 1024×500; this is %d×%d", meta.Width, meta.Height))
		}
	case "tvBanner":
		if meta.Width != 1280 || meta.Height != 720 {
			warnings = append(warnings, fmt.Sprintf("the TV banner must be 1280×720; this is %d×%d", meta.Width, meta.Height))
		}
	default:
		if strings.HasSuffix(imageType, "Screenshots") {
			warnings = append(warnings, screenshotWarnings(meta)...)
		}
	}
	return warnings
}

// screenshotWarnings checks the size and aspect rules that apply to every
// screenshot type.
func screenshotWarnings(meta *imageMeta) []string {
	var warnings []string
	shorter, longer := meta.Width, meta.Height
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	if shorter < minImageDimension || longer > maxImageDimension {
		warnings = append(warnings, fmt.Sprintf("screenshot sides must be between %d and %d pixels; this is %d×%d",
			minImageDimension, maxImageDimension, meta.Width, meta.Height))
	}
	if shorter > 0 && float64(longer)/float64(shorter) > 2 {
		warnings = append(warnings, fmt.Sprintf("a screenshot's aspect ratio may not exceed 2:1; this is %d×%d", meta.Width, meta.Height))
	}
	return warnings
}

// countWarnings reports when a type would end up with more images than Play
// allows. It is separate from imageWarnings because it is a property of the
// set, not of any one file.
func countWarnings(imageType string, count int) []string {
	if strings.HasSuffix(imageType, "Screenshots") && count > maxScreenshots {
		return []string{fmt.Sprintf("%s would hold %d images; Play allows %d", imageType, count, maxScreenshots)}
	}
	if !strings.HasSuffix(imageType, "Screenshots") && count > 1 {
		return []string{fmt.Sprintf("%s holds a single image; %d were given", imageType, count)}
	}
	return nil
}
