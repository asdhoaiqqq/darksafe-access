// This file is the single place the length-counted list *shape* used by the
// audit outputs is defined: whether a list is present at all, how many
// elements it carries, and that every element is written once, in the
// caller's order. The same three list fields — a decision's subject roles,
// a decision's matched-policy ids and a policy change's full policy set —
// travel two output paths: the raw canonical fingerprint encoding for
// records that carry invalid UTF-8, and the lossless archive. Both paths
// used to keep their own copy of the nil/presence-marker, count and
// per-element rules; they now share writeListShape, so the list shape is
// maintained in one place and can never drift between a fingerprint and the
// bytes an archive stores.
//
// What stays output-specific is everything around the shape: the raw
// fingerprint leads a list with its own kind tag (tagStringList versus
// tagPolicyList, so a string list can never collide with a policy list) and
// writes each item as a tagged, length-prefixed value, while the archive
// sits at a fixed field-table position and writes plain length-prefixed
// fields. Only the common shape lives here; each caller supplies the sink
// that frames its own bytes and the function that writes one element.
package darksafe

// List presence is one byte in both outputs. The raw canonical fingerprint
// appends a bare 0 for nil and a bare 1 for a present list immediately after
// the list's own kind tag; the archive writes the same 0/1 values as
// archiveTagNil/archiveTagPresent. Keeping those bytes equal here is what
// lets the two outputs share the shape below without agreeing on anything
// else.
const (
	listMarkerNil     byte = 0
	listMarkerPresent byte = 1
)

// listSink receives the framed bytes of one list. The two outputs implement
// it differently on purpose: the fingerprint sink always appends four raw
// count bytes, while the archive sink routes the count through its
// length-checked u32, so a list length the uint32 archive format cannot
// represent still produces the existing archive error rather than truncated
// framing.
type listSink interface {
	put(b byte)
	listCount(n int)
}

// writeListShape frames every length-counted list in both outputs, whatever
// its item type. A nil slice is exactly one nil marker; a non-nil slice is a
// presence marker, a big-endian uint32 element count and one encoded item
// per element. nil, non-nil empty and populated lists therefore stay three
// distinct shapes, and a populated list keeps its exact length, order and
// duplicates — no sorting, de-duplication or normalization. writeItem owns
// each element's own encoding, so string elements keep their exact bytes
// (Chinese, spaces, control bytes and invalid UTF-8 untouched) in both
// outputs, while the fingerprint's item tags and the archive's plain fields
// remain as each format defines them.
func writeListShape[T any](dst listSink, items []T, writeItem func(T)) {
	if items == nil {
		dst.put(listMarkerNil)
		return
	}
	dst.put(listMarkerPresent)
	dst.listCount(len(items))
	for i := range items {
		writeItem(items[i])
	}
}
