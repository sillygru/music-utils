package ttml

// TimeUnit declares the unit of a raw timing value coming off an upstream
// source, so a provider can never silently hand milliseconds to code that
// expects seconds (or the reverse).
//
// Seconds is the canonical unit for the whole lyrics domain: TTML parsing
// yields seconds, the rich-sync payloads in lyrics_sync_variants are stored in
// seconds, /api/lyrics/get emits seconds in both syncedLyrics and richSync
// content, and the web client reads them as seconds. Providers that speak
// another unit (LyricsPlus mirrors, Paxsenix's raw millisecond fields) must
// declare it and convert at the boundary with ToSeconds.
type TimeUnit string

const (
	// Seconds is the canonical unit used everywhere else in the lyrics domain.
	Seconds TimeUnit = "s"
	// Milliseconds is the unit used by LyricsPlus mirrors and by raw
	// millisecond timing fields from other providers.
	Milliseconds TimeUnit = "ms"
)

// ToSeconds converts v from u into the canonical seconds unit.
func (u TimeUnit) ToSeconds(v float64) float64 {
	if u == Milliseconds {
		return v / 1000
	}
	return v
}

// MillisToSeconds converts a raw millisecond timing value to the canonical
// seconds unit. Callers use it at the provider boundary, where the upstream
// contract is documented in milliseconds.
func MillisToSeconds(v float64) float64 {
	return Milliseconds.ToSeconds(v)
}
