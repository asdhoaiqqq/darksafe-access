package darksafe

import (
	"fmt"
	"unicode/utf8"
)

// 本文件在任何 JSON 解析发生之前校验整行原始文本的编码。
//
// Go 的 encoding/json 对两类损坏输入采取静默修补：非法 UTF-8 字节会被替换成
// U+FFFD；字符串里未配对的 UTF-16 代理项转义（如 \uD800）同样被解码为 U+FFFD。
// 修补之后的文本仍通过 utf8.ValidString 检查，损坏输入因此可能与合法序列的
// 指标名、标签键或标签值身份混淆，造成写入被误判为重复/冲突、查询错误命中。
//
// 因此规则是：每行 JSON 必须整体是合法 UTF-8，且字符串中的 \uXXXX 转义必须
// 表示有效字符——高代理项转义后必须紧接合法的低代理项转义，单独出现的高/低
// 代理项一律拒绝。这类输入作为整行解析失败处理：不带 index、不带 conflict，
// 也不提交该行写入数组中的任何点。

const replacementRuneNote = "corrupt input must not be patched with the replacement character U+FFFD"

// validateLineEncoding 在解析前校验整行原始文本。
// 非法 UTF-8 与非法代理项转义给出可明确区分的错误原因；返回 nil 表示通过。
func validateLineEncoding(line string) *LineError {
	if !utf8.ValidString(line) {
		return &LineError{
			Status: "error",
			Error:  "invalid JSON: input line is not valid UTF-8 text (illegal UTF-8 byte sequence); " + replacementRuneNote,
		}
	}
	if detail := invalidSurrogateEscape(line); detail != "" {
		return &LineError{
			Status: "error",
			Error: fmt.Sprintf(
				"invalid JSON: illegal Unicode surrogate escape in JSON string (%s); surrogate "+
					"escapes must encode a valid character as a \\uD800-\\uDBFF high surrogate "+
					"immediately followed by a \\uDC00-\\uDFFF low surrogate; "+replacementRuneNote,
				detail),
		}
	}
	return nil
}

// invalidSurrogateEscape 扫描原始文本中 JSON 字符串字面量内的 \uXXXX 转义，
// 返回非法代理项的具体原因；没有非法代理项时返回空串。
//
// 扫描在字符串外只跟踪引号：字符串外出现的 \u 本来就不是合法 JSON，交由后续
// 解析拒绝即可。字符串内用反斜杠奇偶判定转义边界，因此 "\\" 之后紧跟的
// uD800（即转义反斜杠后的普通文本）不会被当作代理项转义。
// 形如 \uXXXX 但十六进制位不完整/不合法的序列同样交由 JSON 解析拒绝。
func invalidSurrogateEscape(line string) string {
	inString := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !inString {
			// JSON 字符串只用未转义的双引号定界；字符串外的引号一定是字符串起点。
			if c == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			escaped = false
			if c != 'u' {
				continue
			}
			// i 指向 'u'，十六进制位为 i+1..i+4，其后第一个字节为 i+5。
			r, ok := scanHex4(line, i+1)
			if !ok {
				continue // 畸形 \u 转义：交给 JSON 解析报错
			}
			switch {
			case isHighSurrogate(r):
				// 高代理项后必须紧接另一个完整的低代理项转义。
				low, pairOK := decodeEscapedU(line, i+5)
				if !pairOK || !isLowSurrogate(low) {
					return fmt.Sprintf("high surrogate \\u%04X is not immediately followed by a valid low-surrogate escape", r)
				}
				// 跳过成对的低代理项转义：低转义最后一个十六进制位在 i+10，
				// 循环的 i++ 随后落在 i+11，即整个代理对转义之后。
				i += 10
			case isLowSurrogate(r):
				return fmt.Sprintf("low surrogate \\u%04X has no preceding high-surrogate escape", r)
			default:
				// 普通 \uXXXX：最后一个十六进制位在 i+4，循环 i++ 后落在其后。
				i += 4
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			inString = false
		}
	}
	return ""
}

// decodeEscapedU 检查 line[at:] 是否以完整的 \uXXXX 转义开头，是则返回其码点。
func decodeEscapedU(line string, at int) (rune, bool) {
	if at+6 > len(line) || line[at] != '\\' || line[at+1] != 'u' {
		return 0, false
	}
	return scanHex4(line, at+2)
}

// scanHex4 解析 s[at:at+4] 的四个十六进制位；越界或含非十六进制字符时失败。
func scanHex4(s string, at int) (rune, bool) {
	if at+4 > len(s) {
		return 0, false
	}
	var r rune
	for j := 0; j < 4; j++ {
		c := s[at+j]
		var v rune
		switch {
		case c >= '0' && c <= '9':
			v = rune(c - '0')
		case c >= 'a' && c <= 'f':
			v = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = rune(c-'A') + 10
		default:
			return 0, false
		}
		r = r<<4 | v
	}
	return r, true
}

func isHighSurrogate(r rune) bool { return 0xD800 <= r && r <= 0xDBFF }
func isLowSurrogate(r rune) bool  { return 0xDC00 <= r && r <= 0xDFFF }
