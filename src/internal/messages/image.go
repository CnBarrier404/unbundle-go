package messages

import (
	"encoding/base64"
	"net/http"
	"slices"

	"unbundle-go/src/internal/jsonutil"
)

type ImageBlock struct {
	Type   string `json:"type"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// DecodeImage checks inline encoding and MIME without changing image pixels or
// fetching remote data. Decoded images are bounded separately from HTTP bodies.
func DecodeImage(raw []byte) ([]byte, string, error) {
	var image ImageBlock
	if err := jsonutil.Object(raw, &image); err != nil {
		return nil, "", err
	}
	if image.Type != "image" || image.Source.Type != "base64" || !slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, image.Source.MediaType) {
		return nil, "", unsupported("tool_result.content.image.source")
	}
	if len(image.Source.Data) > base64.StdEncoding.EncodedLen(4<<20) {
		return nil, "", invalid("image", "decoded image exceeds 4 MiB")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(image.Source.Data)
	if err != nil || len(data) == 0 || len(data) > 4<<20 || http.DetectContentType(data) != image.Source.MediaType {
		return nil, "", invalid("image", "invalid image encoding or mismatched MIME type")
	}
	return data, image.Source.MediaType, nil
}

func resultContent(content []byte) error {
	blocks, err := Blocks(content)
	if err != nil {
		return err
	}
	images := 0
	for _, raw := range blocks {
		kind, err := blockType(raw)
		if err != nil {
			return err
		}
		if kind == "image" {
			images++
			if images > 20 {
				return invalid("tool_result.content", "too many images")
			}
			if _, _, err := DecodeImage(raw); err != nil {
				return err
			}
		} else if err := textBlock(raw); err != nil {
			return err
		}
	}
	return nil
}
