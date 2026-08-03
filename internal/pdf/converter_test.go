package pdf

import "testing"

func TestValidRejectsNonPDFAndOversizeDocuments(t *testing.T) {
	if Valid([]byte("not a pdf")) {
		t.Fatal("accepted non-PDF")
	}
	if !Valid([]byte("%PDF-1.7\n%%EOF")) {
		t.Fatal("rejected PDF header")
	}
	if Valid(append([]byte("%PDF-1.7\n%%EOF"), make([]byte, 20<<20)...)) {
		t.Fatal("accepted oversized PDF")
	}
}
