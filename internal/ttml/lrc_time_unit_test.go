package ttml

import "testing"

// The two payloads below are the real stored lyrics for The Pretty Reckless'
// "Only You" and "Only Love Can Save Me Now", both written by the LyricsPlus
// mirror before the millisecond fix in v0.17.0. Every tag past the first minute
// holds the millisecond value formatted as if it were seconds, so 66909ms reads
// as 1115:09.00. They are kept verbatim so a regression shows up against the
// exact strings that reached clients.
const (
	milliOnlyYou = "[00:00.00]Oh, boy, have you seen my head?\n" +
		"[00:44.36]I've lost my mind, so I forget and\n" +
		"[00:49.22]Oh, boy, have you seen my heart?\n" +
		"[00:55.01]It's beating so loud, I'm falling apart and\n" +
		"[00:59.40]Only you can bring me back to life\n" +
		"[1115:09.00]Only you can put me into right\n" +
		"[1199:08.00]Tell me when I can breath again\n" +
		"[1286:24.00]Oh, boy, have you seen my hands?\n" +
		"[3265:54.00]I will arise\n"

	milliOnlyLove = "[00:49.99]Heaven's falling out of the sky\n" +
		"[00:55.64]Sends a message to you, and I\n" +
		"[1015:36.00]See people crawling out of their trees\n" +
		"[1116:18.00]Chained to sickness, the dogs are free\n" +
		"[4550:54.00]Only love, love, love can save me now\n"
)

func TestNormalizeSyncedTimeUnitRepairsMillisecondTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Only You",
			in:   milliOnlyYou,
			want: "[00:00.00]Oh, boy, have you seen my head?\n" +
				"[00:44.36]I've lost my mind, so I forget and\n" +
				"[00:49.22]Oh, boy, have you seen my heart?\n" +
				"[00:55.01]It's beating so loud, I'm falling apart and\n" +
				"[00:59.40]Only you can bring me back to life\n" +
				"[01:06.90]Only you can put me into right\n" +
				"[01:11.94]Tell me when I can breath again\n" +
				"[01:17.18]Oh, boy, have you seen my hands?\n" +
				"[03:15.95]I will arise\n",
		},
		{
			name: "Only Love Can Save Me Now",
			in:   milliOnlyLove,
			want: "[00:49.99]Heaven's falling out of the sky\n" +
				"[00:55.64]Sends a message to you, and I\n" +
				"[01:00.93]See people crawling out of their trees\n" +
				"[01:06.97]Chained to sickness, the dogs are free\n" +
				"[04:33.05]Only love, love, love can save me now\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := NormalizeSyncedTimeUnit(tc.in, MaxPlausibleSyncedSeconds)
			if !changed {
				t.Fatalf("expected the millisecond payload to be rescaled")
			}
			if got != tc.want {
				t.Fatalf("repaired lyrics wrong:\n got %q\nwant %q", got, tc.want)
			}
			// Repairing must be idempotent, or a second pass would keep firing.
			if again, changed := NormalizeSyncedTimeUnit(got, MaxPlausibleSyncedSeconds); changed || again != got {
				t.Fatalf("repair is not idempotent: changed=%v got %q", changed, again)
			}
		})
	}
}

func TestNormalizeSyncedTimeUnitLeavesSecondsAlone(t *testing.T) {
	// A repaired lyric must survive, and so must genuinely long recordings:
	// minute fields below the corrupt floor are never touched, whatever their
	// length, so progressive rock, classical and live sets are unaffected.
	for _, s := range []string{
		"[00:44.36]I've lost my mind so I forget\n[03:18.44]I will arise\n",
		"[01:06.40]Only you can put me into right\n",
		"[00:00.00]start\n[59:59.99]hour long\n",
		"[00:00.00]start\n[119:32.10]ninety-nine minute live set\n",
		"[00:00.00]start\n[999:00.00]sixteen hour radio recording\n",
		"plain untimed lyric\n",
		"",
	} {
		if got, changed := NormalizeSyncedTimeUnit(s, MaxPlausibleSyncedSeconds); changed || got != s {
			t.Fatalf("seconds payload was modified: changed=%v got %q want %q", changed, got, s)
		}
	}
}

func TestNormalizeSyncedTimeUnitRespectsPlausibilityBound(t *testing.T) {
	// The tag is past the corrupt floor, so it looks like milliseconds, but
	// dividing lands at 6000s, beyond what the default bound admits. Leave it
	// alone rather than "repair" a long piece into something shorter than it
	// claims. 100000 minutes is 6000000ms, which is 6000s.
	const in = "[100000:00.00]still counting\n"
	if got, changed := NormalizeSyncedTimeUnit(in, MaxPlausibleSyncedSeconds); changed || got != in {
		t.Fatalf("implausible repair was applied: changed=%v got %q", changed, got)
	}
	// With a bound that admits the value, the same tag is repaired.
	if got, changed := NormalizeSyncedTimeUnit(in, 86400); !changed || got != "[100:00.00]still counting\n" {
		t.Fatalf("expected repair once the bound admits the result: changed=%v got %q", changed, got)
	}
}

func TestNormalizeSyncedTimeUnitOnlyTouchesLeadingTagRun(t *testing.T) {
	// A bracketed number inside lyric text is content, not a timestamp, and
	// mid-line tags fall through as untagged by design.
	for _, s := range []string{
		"hello [1115:09.00] world\n",
		"[00:01.00]they wrote [1115:09.00] on the wall\n",
	} {
		if got, changed := NormalizeSyncedTimeUnit(s, MaxPlausibleSyncedSeconds); changed || got != s {
			t.Fatalf("non-timestamp text was modified: changed=%v got %q want %q", changed, got, s)
		}
	}
}

func TestNormalizeSyncedTimeUnitRepairsRepeatedTagsInOneRun(t *testing.T) {
	got, changed := NormalizeSyncedTimeUnit("[00:44.36][1115:09.00]chorus\n", MaxPlausibleSyncedSeconds)
	if !changed || got != "[00:44.36][01:06.90]chorus\n" {
		t.Fatalf("repeated tag run not repaired: changed=%v got %q", changed, got)
	}
}

func TestLastLRCTagSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"[00:44.36]x\n[01:06.90]y\n", 66.9},
		// Unrepaired millisecond input reports its raw width, which is what
		// makes it detectable in the first place.
		{"[1115:09.00]x\n[03:15.95]y\n", 66909},
		{"no tags here\n", 0},
		{"[00:00.50]x\n", 0.5},
	} {
		if got := LastLRCTagSeconds(tc.in); got != tc.want {
			t.Fatalf("LastLRCTagSeconds(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}