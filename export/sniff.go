package export

import "bytes"

// imagePatterns are the rows of WHATWG MIME Sniffing's image type
// pattern matching algorithm (section 6.1) whose type ATIF takes
// ([atif.ImageMediaTypes]): each pattern, its mask, and the type it
// names. The rows for image/x-icon and image/bmp are left out, as bytes
// matching them render as an image of a type ATIF does not take, which
// is what bytes matching no row do.
var imagePatterns = []struct {
	pattern, mask []byte
	mediaType     string
}{
	{[]byte("GIF87a"), []byte("\xff\xff\xff\xff\xff\xff"), "image/gif"},
	{[]byte("GIF89a"), []byte("\xff\xff\xff\xff\xff\xff"), "image/gif"},
	{[]byte("RIFF\x00\x00\x00\x00WEBPVP"), []byte("\xff\xff\xff\xff\x00\x00\x00\x00\xff\xff\xff\xff\xff\xff"), "image/webp"},
	{[]byte("\x89PNG\r\n\x1a\n"), []byte("\xff\xff\xff\xff\xff\xff\xff\xff"), "image/png"},
	{[]byte("\xff\xd8\xff"), []byte("\xff\xff\xff"), "image/jpeg"},
}

// sniffImage returns the image type data's first bytes match by WHATWG
// MIME Sniffing's image type pattern matching, of the types ATIF takes,
// or "" when they match none: what a projection gives an image part
// whose blob is held with no type (RFC 0001, Projections, ATIF).
func sniffImage(data []byte) string {
	for _, p := range imagePatterns {
		if len(data) < len(p.pattern) {
			continue
		}
		masked := make([]byte, len(p.pattern))
		for i := range p.pattern {
			masked[i] = data[i] & p.mask[i]
		}
		if bytes.Equal(masked, p.pattern) {
			return p.mediaType
		}
	}
	return ""
}
