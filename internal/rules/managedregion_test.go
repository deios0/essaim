package rules

import "testing"

// Codex review: ManagedRegion must skip an ORPHAN solo BEGIN (user-written,
// unpaired) that precedes the real block, and tolerate CRLF fences.
func TestManagedRegionSkipsOrphanBegin(t *testing.T) {
	real := WrapBlock("- rule")
	// A user's standalone solo-line BEGIN in prose, then the real managed block.
	s := "notes\n" + ESSAIM_BEGIN + "\n(the user pasted a bare begin marker above)\n\n" + real + "\ntail\n"
	start, end, ok := ManagedRegion(s)
	if !ok {
		t.Fatal("must find the real block past the orphan BEGIN")
	}
	if s[start:end] != real {
		t.Fatalf("region must be the REAL block, got %q", s[start:end])
	}
}

func TestManagedRegionInlineMarkerIgnored(t *testing.T) {
	s := "we mention `" + ESSAIM_BEGIN + "` inline.\n\n" + WrapBlock("- r") + "\n"
	start, end, ok := ManagedRegion(s)
	if !ok || s[start:end] != WrapBlock("- r") {
		t.Fatalf("inline marker must be ignored; got ok=%v region=%q", ok, s[start:end])
	}
}

func TestManagedRegionCRLF(t *testing.T) {
	crlf := ESSAIM_BEGIN + "\r\n- rule\r\n" + ESSAIM_END + "\r\n"
	s := "# header\r\n\r\n" + crlf
	_, _, ok := ManagedRegion(s)
	if !ok {
		t.Fatal("CRLF-delimited managed fence must be recognized (Windows native files)")
	}
}

func TestManagedRegionNoneWhenAbsent(t *testing.T) {
	if _, _, ok := ManagedRegion("just user content, no fence\n"); ok {
		t.Fatal("must report no region when none present")
	}
	// Inline-only BEGIN, no solo line, no END → not a managed region.
	if _, _, ok := ManagedRegion("prose with " + ESSAIM_BEGIN + " inline only\n"); ok {
		t.Fatal("inline-only BEGIN must not be a managed region")
	}
}
