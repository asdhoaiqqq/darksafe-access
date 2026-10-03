package darksafe

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 本文件回归保障“序列身份”在分隔字符作为用户数据时的归属：
// 冒号、分号、等号、逗号与花括号在指标名、标签键和标签值中都只是普通文本，
// 不能被当成标签之间或键值之间的边界。序列仍由指标名和完整标签集合决定，
// 比较的是 JSON 解析后的真实文本；标签书写顺序与 JSON 转义写法不参与身份判断。
//
// 这里选择的多组输入在朴素的文本拼接身份方案（如 k+"="+v 用逗号连接，或
// k+":"+v 用分号连接）下会发生碰撞，而在长度前缀规范身份下必须各自独立。

// jsonString 以合法 JSON 字符串字面量返回 s：分隔字符 : ; = , { } 与空格都原样
// 保留，只有 JSON 语法必需的字符才被转义。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// identitySample 按给定书写顺序构造一个采样点 JSON；kv 为成对的标签键、标签值，
// 其中的分隔字符与空格都作为用户数据原样写入。
func identitySample(name string, ts int64, value float64, kv ...string) string {
	if len(kv)%2 != 0 {
		panic("identitySample: kv must be provided as key/value pairs")
	}
	var b strings.Builder
	b.WriteString(`{"name":`)
	b.WriteString(jsonString(name))
	b.WriteString(`,"timestamp":`)
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteString(`,"value":`)
	b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	b.WriteString(`,"labels":{`)
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(kv[i]))
		b.WriteByte(':')
		b.WriteString(jsonString(kv[i+1]))
	}
	b.WriteString("}}")
	return b.String()
}

// identityBatch 把若干采样点 JSON 拼成一个写入批次。
func identityBatch(samples ...string) string {
	return "[" + strings.Join(samples, ",") + "]"
}

// identityQuery 构造一个区间覆盖全部测试时间戳的子集查询；kv 省略时匹配全部序列。
func identityQuery(name string, kv ...string) string {
	var b strings.Builder
	b.WriteString(`{"op":"query","name":`)
	b.WriteString(jsonString(name))
	b.WriteString(`,"start":0,"end":9000000000000000000`)
	if len(kv) > 0 {
		if len(kv)%2 != 0 {
			panic("identityQuery: kv must be provided as key/value pairs")
		}
		b.WriteString(`,"labels":{`)
		for i := 0; i < len(kv); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(kv[i]))
			b.WriteByte(':')
			b.WriteString(jsonString(kv[i+1]))
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.String()
}

// countViews 统计写入快照中名称与完整标签集合完全相等的序列条数。
func countViews(res *BatchResult, name string, labels map[string]string) int {
	n := 0
	for _, v := range res.Series {
		if v.Name == name && reflect.DeepEqual(v.Labels, labels) {
			n++
		}
	}
	return n
}

// expectExactlyOneQuery 断言查询结果中恰有一条名称与完整标签集合完全相等的序列，
// 并返回该条目；其余条目的存在与否由调用方另行约束。
func expectExactlyOneQuery(t *testing.T, res *QueryResult, name string, labels map[string]string) QuerySeries {
	t.Helper()
	var found QuerySeries
	n := 0
	for _, q := range res.Series {
		if q.Name == name && reflect.DeepEqual(q.Labels, labels) {
			found = q
			n++
		}
	}
	if n != 1 {
		t.Fatalf("query for %s %v matched %d entries in %+v, want exactly 1", name, labels, n, res.Series)
	}
	return found
}

// TestDelimiterCharactersInLabelValuesKeepSeriesDistinct 覆盖每个分隔字符出现在
// 标签值中的情形：即使不同序列在同一时间戳写入相同数值，也必须分别新增；
// 之后写入不同数值只与真实同身份序列冲突，子集查询各归各的点。
func TestDelimiterCharactersInLabelValuesKeepSeriesDistinct(t *testing.T) {
	for _, ch := range []string{":", ";", "=", ",", "{", "}", ",;=:{}"} {
		t.Run("char="+ch, func(t *testing.T) {
			s := NewMetricStore()
			vA := "x" + ch + "y"
			vB := "p" + ch + "q"
			// sA={host:vA}，sB={host:vB}（同键不同值），sC 是 sA 的标签超集。
			res := mustOK(t, s, identityBatch(
				identitySample("m", 1, 1, "host", vA),
				identitySample("m", 1, 1, "host", vB),
				identitySample("m", 1, 1, "host", vA, "zone", "z"),
			))
			if res.Added != 3 || res.Duplicates != 0 || len(res.Series) != 3 {
				t.Fatalf("initial batch = %+v, want 3 distinct series", res)
			}

			// 同值再次提交各自仍是重复，不新增点。
			if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 1, "host", vA))); dup.Added != 0 || dup.Duplicates != 1 {
				t.Fatalf("sA same value = %+v, want one duplicate", dup)
			}
			if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 1, "host", vB))); dup.Added != 0 || dup.Duplicates != 1 {
				t.Fatalf("sB same value = %+v, want one duplicate", dup)
			}

			// sA 提交不同值：冲突必须指向单标签的真实 sA，而不是值文本相近的 sB/sC。
			lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 2, "host", vA)))
			if lerr.Index != 1 || lerr.Conflict == nil {
				t.Fatalf("sA conflict = %+v", lerr)
			}
			c := lerr.Conflict
			if c.Series.Name != "m" || !reflect.DeepEqual(c.Series.Labels, map[string]string{"host": vA}) ||
				len(c.Series.Labels) != 1 || c.Timestamp != 1 || c.Existing != 1 || c.Submitted != 2 {
				t.Fatalf("sA conflict detail = %+v, want the real single-label series", c)
			}
			// 冲突拒绝整批：sA 原值保持，sB/sC 也不受影响。
			qr := mustQuery(t, s, identityQuery("m", "host", vA))
			if got := expectExactlyOneQuery(t, qr, "m", map[string]string{"host": vA, "zone": "z"}); got.Count != 1 || got.Average != 1 {
				t.Fatalf("sC = %+v, want count=1 average=1", got)
			}
			if len(qr.Series) != 2 {
				t.Fatalf("subset host=vA must match sA and sC only, got %+v", qr.Series)
			}
			for _, q := range qr.Series {
				if q.Count != 1 || q.Average != 1 {
					t.Fatalf("rejected value must not land on any series: %+v", q)
				}
			}
			if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "host", vB)), "m",
				map[string]string{"host": vB}); got.Count != 1 || got.Average != 1 {
				t.Fatalf("sB fact changed after sA conflict: %+v", got)
			}
			if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "zone", "z")), "m",
				map[string]string{"host": vA, "zone": "z"}); got.Count != 1 {
				t.Fatalf("sC zone match = %+v", got)
			}
		})
	}
}

// TestLabelValueMasqueradingAsTwoLabels 是核心混淆用例：
// 单标签 s1 的值 "a,x=d" 在 k=v、逗号连接的文本方案里看起来恰好等于两个标签
// （host=a 与 x=d）。真实身份下单标签序列与双标签序列必须始终分开。
func TestLabelValueMasqueradingAsTwoLabels(t *testing.T) {
	s := NewMetricStore()
	hostOnly := map[string]string{"host": "a,x=d"}
	twoLabels := map[string]string{"host": "a", "x": "d"}

	// 同一时间戳、相同数值：仍是两条序列。
	res := mustOK(t, s, identityBatch(
		identitySample("m", 1, 10, "host", "a,x=d"),
		identitySample("m", 1, 10, "host", "a", "x", "d"),
	))
	if res.Added != 2 || res.Duplicates != 0 || len(res.Series) != 2 {
		t.Fatalf("initial batch = %+v, want 2 distinct series", res)
	}
	if v := res.Series[findViewIndex(t, res, "m", hostOnly)]; len(v.Labels) != 1 || v.Labels["host"] != "a,x=d" {
		t.Fatalf("s1 labels not preserved verbatim: %+v", v)
	}
	if v := res.Series[findViewIndex(t, res, "m", twoLabels)]; len(v.Labels) != 2 ||
		v.Labels["host"] != "a" || v.Labels["x"] != "d" {
		t.Fatalf("s2 labels not preserved: %+v", v)
	}

	// 同值重复：两条序列各自识别自己，不新增点。
	if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 10, "host", "a,x=d"))); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("s1 duplicate = %+v", dup)
	}
	if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 10, "x", "d", "host", "a"))); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("s2 duplicate in different key order = %+v", dup)
	}

	// s1 再写一个新时间戳，制造“点数不同”的局面。
	mustOK(t, s, identityBatch(identitySample("m", 2, 30, "host", "a,x=d")))

	// 不同值冲突必须指出真实的完整序列：s1 只有一个标签键。
	lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 20, "host", "a,x=d")))
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("s1 conflict = %+v", lerr)
	}
	c := lerr.Conflict
	if !reflect.DeepEqual(c.Series.Labels, hostOnly) || len(c.Series.Labels) != 1 ||
		c.Timestamp != 1 || c.Existing != 10 || c.Submitted != 20 {
		t.Fatalf("s1 conflict = %+v, want the single-label series existing=10 submitted=20", c)
	}
	// s2 的冲突详情必须是两个标签，existing 仍是 10。
	lerr = mustFail(t, s, identityBatch(identitySample("m", 1, 30, "host", "a", "x", "d")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, twoLabels) ||
		len(lerr.Conflict.Series.Labels) != 2 || lerr.Conflict.Existing != 10 || lerr.Conflict.Submitted != 30 {
		t.Fatalf("s2 conflict = %+v, want both labels existing=10", lerr.Conflict)
	}

	// 子集查询：标签文本里的逗号和等号绝不允许被拆成另一个标签。
	q := mustQuery(t, s, identityQuery("m", "host", "a,x=d"))
	if len(q.Series) != 1 {
		t.Fatalf("host=a,x=d subset = %+v, want only s1", q.Series)
	}
	if got := expectExactlyOneQuery(t, q, "m", hostOnly); got.Count != 2 || got.Average != 20 {
		// (10+30)/2 = 20：s1 自己两个点；s2 的点绝不能混入。
		t.Fatalf("s1 query = %+v, want count=2 average=20", got)
	}
	// host=a 只匹配 s2：s1 的真实值是 "a,x=d"，不是 "a"。
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "host", "a")), "m", twoLabels); got.Count != 1 || got.Average != 10 {
		t.Fatalf("host=a subset = %+v, want only s2 with one point", got)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "x", "d")), "m", twoLabels); got.Count != 1 || got.Average != 10 {
		t.Fatalf("x=d subset = %+v, want only s2", got)
	}
	if qr := mustQuery(t, s, identityQuery("m", "host", "a,x=d", "x", "d")); len(qr.Series) != 0 {
		t.Fatalf("requiring both split labels must not match s1: %+v", qr.Series)
	}
	// 全量查询仍是两条序列，各自点数与均值独立。
	full := mustQuery(t, s, identityQuery("m"))
	if len(full.Series) != 2 {
		t.Fatalf("full query = %+v, want 2 series", full.Series)
	}
	expectExactlyOneQuery(t, full, "m", hostOnly)
	expectExactlyOneQuery(t, full, "m", twoLabels)
}

// TestKeyValueBoundaryAmbiguityWithColonAndSemicolon 覆盖不同的“键值划分”
// 在冒号/分号文本方案下看起来像同一段字符串的情形：
// s1 两个标签（host=1 与 "x:2"=y）的无长度拼接 "host:1;x:2:y;" 与
// s2 单个标签 host="1;x:2:y" 的拼接完全相同；真实身份必须区分。
func TestKeyValueBoundaryAmbiguityWithColonAndSemicolon(t *testing.T) {
	s := NewMetricStore()
	twoPairs := map[string]string{"host": "1", "x:2": "y"}
	onePair := map[string]string{"host": "1;x:2:y"}

	res := mustOK(t, s, identityBatch(
		identitySample("m", 1, 7, "host", "1", "x:2", "y"),
		identitySample("m", 1, 7, "host", "1;x:2:y"),
	))
	if res.Added != 2 || len(res.Series) != 2 {
		t.Fatalf("initial batch = %+v, want 2 series", res)
	}

	// 同值重复各自识别。
	if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 7, "x:2", "y", "host", "1"))); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("s1 reordered duplicate = %+v", dup)
	}
	if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 7, "host", "1;x:2:y"))); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("s2 duplicate = %+v", dup)
	}

	// 异值冲突归属：s1 报两个标签，s2 报一个标签。
	lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 8, "host", "1", "x:2", "y")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, twoPairs) ||
		len(lerr.Conflict.Series.Labels) != 2 || lerr.Conflict.Existing != 7 || lerr.Conflict.Submitted != 8 {
		t.Fatalf("s1 conflict = %+v, want two real pairs", lerr.Conflict)
	}
	lerr = mustFail(t, s, identityBatch(identitySample("m", 1, 9, "host", "1;x:2:y")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, onePair) ||
		len(lerr.Conflict.Series.Labels) != 1 || lerr.Conflict.Existing != 7 || lerr.Conflict.Submitted != 9 {
		t.Fatalf("s2 conflict = %+v, want one real pair", lerr.Conflict)
	}

	// 子集匹配不能在冒号分号处重新切分。
	if qr := mustQuery(t, s, identityQuery("m", "host", "1")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, twoPairs) || qr.Series[0].Average != 7 {
		t.Fatalf("host=1 must match only the two-pair series: %+v", qr.Series)
	}
	if qr := mustQuery(t, s, identityQuery("m", "x:2", "y")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, twoPairs) {
		t.Fatalf(`"x:2"=y subset = %+v, want s1`, qr.Series)
	}
	if qr := mustQuery(t, s, identityQuery("m", "host", "1;x:2:y")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, onePair) {
		t.Fatalf("host with semicolon value subset = %+v, want s2", qr.Series)
	}
	if len(mustQuery(t, s, identityQuery("m")).Series) != 2 {
		t.Fatalf("full query must keep both series")
	}
}

// TestDelimiterCharactersInLabelKeys 覆盖分隔字符出现在标签键中，以及
// 键值等号歧义：{"a=b":"c"} 与 {"a":"b=c"} 的 k=v 文本都是 a=b=c，
// 但完整标签集合不同，必须是两条序列；子集查询按键的真实文本精确归属。
func TestDelimiterCharactersInLabelKeys(t *testing.T) {
	for _, ch := range []string{":", ";", ",", "{", "}", "{:;},}"} {
		t.Run("char="+ch, func(t *testing.T) {
			s := NewMetricStore()
			funnyKey := "a" + ch + "b"
			res := mustOK(t, s, identityBatch(
				identitySample("m", 1, 1, funnyKey, "v"),
				identitySample("m", 1, 1, "a", "v"),
			))
			if res.Added != 2 || len(res.Series) != 2 {
				t.Fatalf("initial batch = %+v, want 2 series", res)
			}
			if qr := mustQuery(t, s, identityQuery("m", funnyKey, "v")); len(qr.Series) != 1 ||
				!reflect.DeepEqual(qr.Series[0].Labels, map[string]string{funnyKey: "v"}) {
				t.Fatalf("funny-key subset = %+v", qr.Series)
			}
			if qr := mustQuery(t, s, identityQuery("m", "a", "v")); len(qr.Series) != 1 ||
				!reflect.DeepEqual(qr.Series[0].Labels, map[string]string{"a": "v"}) {
				t.Fatalf("plain-key subset = %+v", qr.Series)
			}
			lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 2, funnyKey, "v")))
			if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, map[string]string{funnyKey: "v"}) ||
				lerr.Conflict.Existing != 1 {
				t.Fatalf("funny-key conflict = %+v", lerr.Conflict)
			}
		})
	}

	// 等号在键中：两种键值划分产生同一段 k=v 文本。
	s := NewMetricStore()
	keyHasEquals := map[string]string{"a=b": "c"}
	valHasEquals := map[string]string{"a": "b=c"}
	res := mustOK(t, s, identityBatch(
		identitySample("m", 1, 4, "a=b", "c"),
		identitySample("m", 1, 4, "a", "b=c"),
	))
	if res.Added != 2 || len(res.Series) != 2 {
		t.Fatalf("equals ambiguity batch = %+v, want 2 series", res)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a=b", "c")), "m", keyHasEquals); got.Average != 4 {
		t.Fatalf("key-with-equals subset = %+v", got)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a", "b=c")), "m", valHasEquals); got.Average != 4 {
		t.Fatalf("value-with-equals subset = %+v", got)
	}
	if qr := mustQuery(t, s, identityQuery("m", "a", "c")); len(qr.Series) != 0 {
		t.Fatalf("a=c matches neither split: %+v", qr.Series)
	}
	lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 5, "a=b", "c")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, keyHasEquals) ||
		len(lerr.Conflict.Series.Labels) != 1 || lerr.Conflict.Existing != 4 || lerr.Conflict.Submitted != 5 {
		t.Fatalf("key-with-equals conflict = %+v", lerr.Conflict)
	}
	lerr = mustFail(t, s, identityBatch(identitySample("m", 1, 6, "a", "b=c")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, valHasEquals) ||
		lerr.Conflict.Existing != 4 || lerr.Conflict.Submitted != 6 {
		t.Fatalf("value-with-equals conflict = %+v", lerr.Conflict)
	}
}

// TestMetricNameDelimitersPartOfIdentity 覆盖分隔字符出现在指标名中：
// 名称是逐字符的真实文本；换用 JSON 转义书写同一名称仍是同一序列，
// 不同名称即使同时间戳同值也分别新增、互不冲突。
func TestMetricNameDelimitersPartOfIdentity(t *testing.T) {
	s := NewMetricStore()
	n1 := "m:;={},x"
	n2 := "m:;={},y"
	res := mustOK(t, s, identityBatch(
		identitySample(n1, 1, 1),
		identitySample(n2, 1, 1),
	))
	if res.Added != 2 || len(res.Series) != 2 {
		t.Fatalf("initial batch = %+v, want 2 differently named series", res)
	}

	// 同一名称，全部改用 \uXXXX 转义书写其中的分隔字符：同一身份、同值重复。
	escaped := `[{"name":"m:;={},x","timestamp":1,"value":1.0}]`
	if dup := mustOK(t, s, escaped); dup.Added != 0 || dup.Duplicates != 1 || len(dup.Series) != 2 {
		t.Fatalf("escaped-name resubmit = %+v, want one duplicate", dup)
	}

	// 不同名称提交不同值不构成冲突（各自新增自己的点）。
	if add := mustOK(t, s, identityBatch(identitySample(n2, 2, 3))); add.Added != 1 {
		t.Fatalf("n2 new timestamp = %+v, want added=1", add)
	}

	// 对 n1 提交不同值：冲突详情中的名称必须是真实的完整 n1。
	lerr := mustFail(t, s, identityBatch(identitySample(n1, 1, 2)))
	if lerr.Conflict == nil || lerr.Conflict.Series.Name != n1 ||
		len(lerr.Conflict.Series.Labels) != 0 || lerr.Conflict.Timestamp != 1 ||
		lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("n1 conflict = %+v", lerr.Conflict)
	}
	if !strings.Contains(lerr.Error, n1) {
		t.Fatalf("conflict message %q must contain the full real name %q", lerr.Error, n1)
	}

	// 名称精确匹配：n1 仍是原值 1 的一个点；n2 有自己的两个点。
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery(n1)), n1, map[string]string{}); got.Count != 1 || got.Average != 1 {
		t.Fatalf("n1 query = %+v, want its own untouched point", got)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery(n2)), n2, map[string]string{}); got.Count != 2 || got.Average != 2 {
		t.Fatalf("n2 query = %+v, want count=2 average=2", got)
	}
}

// TestSameIdentityAcrossLabelOrderAndJSONEscapes 保护身份确实相同的一面：
// 调整标签书写顺序，并用合法 JSON 转义书写值与名称中的同一批分隔字符，
// 同值提交计为重复；不同值拒绝整批，冲突指出真实完整序列、时间戳与两个值，
// 原采样点保持不变。
func TestSameIdentityAcrossLabelOrderAndJSONEscapes(t *testing.T) {
	s := NewMetricStore()
	realLabels := map[string]string{"host": "a;b=c,d{e}", "zone": "z:1"}
	mustOK(t, s, identityBatch(
		identitySample("m:x", 1, 1, "host", "a;b=c,d{e}", "zone", "z:1"),
	))

	// 颠倒标签顺序；名称冒号与值中的 : ; = , { } 全部改用 \uXXXX 书写同一字符。
	escaped := `[{"name":"m:x","timestamp":1,"value":1.0,` +
		`"labels":{"zone":"z:1","host":"a;b=c,d{e}"}}]`
	if dup := mustOK(t, s, escaped); dup.Added != 0 || dup.Duplicates != 1 || len(dup.Series) != 1 {
		t.Fatalf("reordered + escaped resubmit = %+v, want a single duplicate", dup)
	}

	// 同身份不同值：整批拒绝，冲突详情给出解析后的真实文本。
	escapedConflict := `[{"name":"m:x","timestamp":1,"value":2,` +
		`"labels":{"zone":"z:1","host":"a;b=c,d{e}"}}]`
	lerr := mustFail(t, s, escapedConflict)
	if lerr.Index != 1 || lerr.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", lerr)
	}
	c := lerr.Conflict
	if c.Series.Name != "m:x" || !reflect.DeepEqual(c.Series.Labels, realLabels) ||
		c.Timestamp != 1 || c.Existing != 1 || c.Submitted != 2 {
		t.Fatalf("conflict detail = %+v, want the real full identity and both values", c)
	}

	// 原采样点保持不变：仍只有一个点、均值 1，名称与标签为真实文本。
	qr := mustQuery(t, s, identityQuery("m:x", "host", "a;b=c,d{e}"))
	got := expectExactlyOneQuery(t, qr, "m:x", realLabels)
	if got.Count != 1 || got.Average != 1 {
		t.Fatalf("original point changed after rejected batch: %+v", got)
	}
	snap := mustOK(t, s, `[]`)
	v := snap.Series[findViewIndex(t, snap, "m:x", realLabels)]
	if len(v.Points) != 1 || v.Points[0] != (Point{Timestamp: 1, Value: 1}) {
		t.Fatalf("stored point = %+v, want only (1,1)", v.Points)
	}
}

// TestOmittedEmptyAndEmptyValueLabelsWithDelimiterText 保留两个身份边界在
// 分隔字符场景下的表现：省略标签与空对象同一身份；缺少标签与标签存在但值为空串
// 是不同身份；空串值与“看起来空”的分隔字符值（分号、冒号）也必须分开。
func TestOmittedEmptyAndEmptyValueLabelsWithDelimiterText(t *testing.T) {
	s := NewMetricStore()
	noLabels := map[string]string{}
	emptyVal := map[string]string{"k": ""}
	semicolonVal := map[string]string{"k": ";"}
	colonVal := map[string]string{"k": ":"}

	// 四条不同身份在同一时间戳写相同值：无标签、k=""、k=";"、k=":"。
	mustOK(t, s, `[{"name":"m","timestamp":1,"value":1}]`)
	res := mustOK(t, s, identityBatch(
		identitySample("m", 1, 1, "k", ""),
		identitySample("m", 1, 1, "k", ";"),
		identitySample("m", 1, 1, "k", ":"),
	))
	if res.Added != 3 || len(res.Series) != 4 {
		t.Fatalf("initial series = %+v, want 4 distinct identities", res)
	}
	// 空对象与省略标签同一身份：写入无标签序列的新时间戳，而不是新增序列。
	res = mustOK(t, s, `[{"name":"m","timestamp":2,"value":5,"labels":{}}]`)
	if res.Added != 1 || len(res.Series) != 4 {
		t.Fatalf("empty labels object must equal omitted labels, got %+v", res)
	}

	// 子集查询各自只命中真实身份。
	if qr := mustQuery(t, s, identityQuery("m", "k", "")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, emptyVal) || qr.Series[0].Count != 1 {
		t.Fatalf("k='' subset = %+v, want only the empty-value series", qr.Series)
	}
	if qr := mustQuery(t, s, identityQuery("m", "k", ";")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, semicolonVal) {
		t.Fatalf("k=';' subset = %+v", qr.Series)
	}
	if qr := mustQuery(t, s, identityQuery("m", "k", ":")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, colonVal) {
		t.Fatalf("k=':' subset = %+v", qr.Series)
	}
	full := mustQuery(t, s, identityQuery("m"))
	if len(full.Series) != 4 {
		t.Fatalf("full query = %+v, want 4 series", full.Series)
	}
	expectExactlyOneQuery(t, full, "m", noLabels)
	expectExactlyOneQuery(t, full, "m", emptyVal)
	expectExactlyOneQuery(t, full, "m", semicolonVal)
	expectExactlyOneQuery(t, full, "m", colonVal)

	// 空值序列的同值重复与异值冲突都带着真实存在的空串标签。
	if dup := mustOK(t, s, identityBatch(identitySample("m", 1, 1, "k", ""))); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("empty-value duplicate = %+v", dup)
	}
	lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 9, "k", "")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, emptyVal) ||
		lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 9 {
		t.Fatalf("empty-value conflict = %+v", lerr.Conflict)
	}
}

// TestWhitespaceInIdentityPreservedVerbatim 保护名称、标签键和值中的合法空格
// 原样参与身份：不得通过去掉或合并空格把不同输入当成同一序列；
// 用 转义书写的空格与直接书写是同一身份。
func TestWhitespaceInIdentityPreservedVerbatim(t *testing.T) {
	s := NewMetricStore()
	// 指标名：单空格、双空格、尾随空格各不相同。
	res := mustOK(t, s, identityBatch(
		identitySample("metric x", 1, 1),
		identitySample("metric  x", 1, 1),
		identitySample("metric x ", 1, 1),
	))
	if res.Added != 3 || len(res.Series) != 3 {
		t.Fatalf("metric names with spaces = %+v, want 3 series", res)
	}
	// "metric x" 与直接书写的单空格名称同身份、同值重复。
	if dup := mustOK(t, s, `[{"name":"metric x","timestamp":1,"value":1.0}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("escaped-space name duplicate = %+v", dup)
	}
	for _, n := range []string{"metric x", "metric  x", "metric x "} {
		if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery(n)), n, map[string]string{}); got.Count != 1 || got.Average != 1 {
			t.Fatalf("name %q query = %+v", n, got)
		}
	}

	// 标签键与标签值中的空格：单空格、双空格、前导/尾随空格都是不同身份。
	v1 := map[string]string{"a b": "x y"}
	v2 := map[string]string{"a b ": "x y"} // 键带尾随空格
	v3 := map[string]string{"a b": "x  y"} // 值双空格
	res = mustOK(t, s, identityBatch(
		identitySample("m", 1, 1, "a b", "x y"),
		identitySample("m", 1, 1, "a b ", "x y"),
		identitySample("m", 1, 1, "a b", "x  y"),
	))
	// 快照列出全部序列：3 个名称序列加 3 个标签序列共 6 条；本批新增恰为 3。
	if res.Added != 3 || res.Duplicates != 0 || len(res.Series) != 6 {
		t.Fatalf("label whitespace batch = added=%d series=%d, want added=3 and 6 total series",
			res.Added, len(res.Series))
	}
	// 空格以 转义书写仍是同一身份：同值重复（这里直接写 JSON 转义，
	// 确认解析后的真实文本参与身份判断）。
	if dup := mustOK(t, s, `[{"name":"m","timestamp":1,"value":1.0,`+
		`"labels":{"a b":"x y"}}]`); dup.Added != 0 || dup.Duplicates != 1 {
		t.Fatalf("escaped-space label duplicate = %+v", dup)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a b", "x y")), "m", v1); got.Count != 1 {
		t.Fatalf("single-space subset = %+v", got)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a b ", "x y")), "m", v2); got.Count != 1 || got.Average != 1 {
		t.Fatalf("trailing-space key subset = %+v", got)
	}
	if got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a b", "x  y")), "m", v3); got.Count != 1 || got.Average != 1 {
		t.Fatalf("double-space value subset = %+v", got)
	}
	if qr := mustQuery(t, s, identityQuery("m", "a b", "xy")); len(qr.Series) != 0 {
		t.Fatalf("whitespace must not be trimmed into a match: %+v", qr.Series)
	}
	if qr := mustQuery(t, s, identityQuery("m", "ab", "x y")); len(qr.Series) != 0 {
		t.Fatalf("whitespace in a key must not be trimmed: %+v", qr.Series)
	}
	lerr := mustFail(t, s, identityBatch(identitySample("m", 1, 2, "a b", "x y")))
	if lerr.Conflict == nil || !reflect.DeepEqual(lerr.Conflict.Series.Labels, v1) ||
		lerr.Conflict.Existing != 1 || lerr.Conflict.Submitted != 2 {
		t.Fatalf("whitespace identity conflict = %+v", lerr.Conflict)
	}
}

// TestSubsetMatchTreatsDelimiterTextAsAtomicLabelValues 从查询侧回归：
// 一个标签值里可以出现成段的 "键=值;键:值," 文本，但子集匹配只按完整标签
// 集合逐键精确比较，绝不把值文本的一部分当成另一个标签；不同序列的点不混。
func TestSubsetMatchTreatsDelimiterTextAsAtomicLabelValues(t *testing.T) {
	s := NewMetricStore()
	richLabels := map[string]string{"a:b": "x=y", "c": "{d=e;f:g,}"}
	decoyLabels := map[string]string{"a:b": "x", "d": "e"}
	mustOK(t, s, identityBatch(
		identitySample("m", 1, 2, "a:b", "x=y", "c", "{d=e;f:g,}"),
		identitySample("m", 2, 4, "c", "{d=e;f:g,}", "a:b", "x=y"),
		identitySample("m", 1, 99, "a:b", "x", "d", "e"),
	))

	// 富文本序列两个点 (2+4)/2=3；干扰序列一个点 99，二者绝不混算。
	got := expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "a:b", "x=y")), "m", richLabels)
	if got.Count != 2 || got.Average != 3 {
		t.Fatalf("rich series subset = %+v, want count=2 average=3", got)
	}
	got = expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "c", "{d=e;f:g,}")), "m", richLabels)
	if got.Count != 2 || got.Average != 3 {
		t.Fatalf("brace/semicolon value subset = %+v", got)
	}
	// 干扰序列只被自己的真实标签命中。
	got = expectExactlyOneQuery(t, mustQuery(t, s, identityQuery("m", "d", "e")), "m", decoyLabels)
	if got.Count != 1 || got.Average != 99 {
		t.Fatalf("decoy subset = %+v, want count=1 average=99", got)
	}
	// 值文本里出现的 "x"、"x=y"、"{d=e;f:g,}" 等都必须作为完整原子值参与匹配：
	// a:b=x 只命中真实拥有该值的干扰序列，不能同时命中值为 "x=y" 的富文本序列；
	// x=y、d=e 这类嵌在值文本中的片段都不是标签，对应查询必须为空。
	if qr := mustQuery(t, s, identityQuery("m", "a:b", "x")); len(qr.Series) != 1 ||
		!reflect.DeepEqual(qr.Series[0].Labels, decoyLabels) {
		t.Fatalf("a:b=x must hit only the decoy series, not the rich x=y series: %+v", qr.Series)
	}
	for _, q := range []string{
		identityQuery("m", "x", "y"),
		identityQuery("m", "d", "e", "c", "{d=e;f:g,}"),
	} {
		if qr := mustQuery(t, s, q); len(qr.Series) != 0 {
			t.Fatalf("query %s must not split label text into labels: %+v", q, qr.Series)
		}
	}
	if len(mustQuery(t, s, identityQuery("m")).Series) != 2 {
		t.Fatalf("full query must list exactly the two real series")
	}
}
