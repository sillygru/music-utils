package httpserver

import "fmt"

func mergeLyricsVariants(existing, incoming *lyricsResponse) {
	if existing == nil || incoming == nil {
		return
	}
	for _, variant := range incoming.Variants {
		appendVariant(existing, variant)
	}
	if incoming.RichSync != nil || incoming.PlainLyrics != "" || incoming.SyncedLyrics != "" || incoming.Instrumental {
		appendLyricsVariant(existing, incoming)
	}
}

func appendLyricsVariant(response *lyricsResponse, incoming *lyricsResponse) {
	if response == nil || incoming == nil {
		return
	}
	variant := lyricsVariant{Provider: "lrclib", Format: "lrc", PlainLyrics: incoming.PlainLyrics, SyncedLyrics: incoming.SyncedLyrics}
	if incoming.RichSync != nil {
		variant.Provider = incoming.RichSync.Source
		variant.SyncType = incoming.RichSync.SyncType
		variant.Format = incoming.RichSync.Format
		variant.RichSync = incoming.RichSync
	}
	if variant.SyncType == "" {
		if incoming.SyncedLyrics != "" {
			variant.SyncType = "line"
		} else {
			variant.SyncType = "plain"
		}
	}
	for _, current := range response.Variants {
		if current.Provider == variant.Provider && current.SyncType == variant.SyncType && current.Format == variant.Format && fmt.Sprint(current.RichSync) == fmt.Sprint(variant.RichSync) && current.PlainLyrics == variant.PlainLyrics && current.SyncedLyrics == variant.SyncedLyrics {
			return
		}
	}
	appendVariant(response, variant)
}

func appendVariant(response *lyricsResponse, variant lyricsVariant) {
	if response == nil {
		return
	}
	for _, current := range response.Variants {
		if current.Provider == variant.Provider && current.SyncType == variant.SyncType && current.Format == variant.Format && current.PlainLyrics == variant.PlainLyrics && current.SyncedLyrics == variant.SyncedLyrics {
			return
		}
	}
	response.Variants = append(response.Variants, variant)
}
