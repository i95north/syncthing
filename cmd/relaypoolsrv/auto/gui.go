package auto

import (
	"encoding/base64"
)

const (
	AssetsBuildDate = "Fri, 23 Oct 2015 23:02:30 GMT"
)

func Assets() map[string][]byte {
	var assets = make(map[string][]byte, 1)

	assets["index.html"], _ = base64.StdEncoding.DecodeString("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")
	return assets
}
