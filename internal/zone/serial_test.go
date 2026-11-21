package zone

import "testing"

func TestCompareSerial(t *testing.T) {
	const half = uint32(1) << 31
	cases := []struct {
		name string
		a, b uint32
		want SerialRelation
	}{
		{"相等", 5, 5, SerialEqual},
		{"零相等", 0, 0, SerialEqual},
		{"普通小于", 1, 2, SerialLess},
		{"普通大于", 2, 1, SerialGreater},
		{"大跨度小于", 100, 100 + half - 1, SerialLess},
		{"大跨度大于", 100 + half - 1, 100, SerialGreater},
		{"回绕后仍小于", 4294967290, 6, SerialLess},   // 4294967290 → 6 差 12
		{"回绕边界加一", 4294967295, 0, SerialLess},   // 最大值 +1 回绕到 0
		{"回绕后大于", 6, 4294967290, SerialGreater}, // 反向看同一对值
		{"临界差不确定", 0, half, SerialUncertain},    // 差值恰为 2^31
		{"临界差不确定反向", half, 0, SerialUncertain},  // 对称同样不确定
		{"临界差不确定大数", 2024010101, 2024010101 + half, SerialUncertain},
		{"临界减一可比较", 0, half - 1, SerialLess},
		{"临界加一可比较", 0, half + 1, SerialGreater}, // 差 2^31+1 → 反向
	}
	for _, c := range cases {
		if got := CompareSerial(c.a, c.b); got != c.want {
			t.Errorf("%s: CompareSerial(%d, %d) = %v, 期望 %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestCompareSerialSymmetry(t *testing.T) {
	// 除“不确定”外，交换参数应得到相反关系。
	pairs := [][2]uint32{{1, 2}, {4294967290, 6}, {100, 3000000000}, {7, 7}}
	for _, p := range pairs {
		fwd := CompareSerial(p[0], p[1])
		rev := CompareSerial(p[1], p[0])
		switch fwd {
		case SerialLess:
			if rev != SerialGreater {
				t.Errorf("CompareSerial(%d,%d)=less 但反向=%v", p[0], p[1], rev)
			}
		case SerialGreater:
			if rev != SerialLess {
				t.Errorf("CompareSerial(%d,%d)=greater 但反向=%v", p[0], p[1], rev)
			}
		case SerialEqual:
			if rev != SerialEqual {
				t.Errorf("CompareSerial(%d,%d)=equal 但反向=%v", p[0], p[1], rev)
			}
		}
	}
}
