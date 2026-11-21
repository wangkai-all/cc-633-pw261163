package zone

// RFC 1982 定义的 32 位序列号算术。
//
// 区序列号是 32 位无符号整数，比较时不能直接用普通大小关系：
// 值域会回绕，两个序列号之差（模 2^32）小于 2^31 时才能确定先后；
// 差值恰好等于 2^31 时，RFC 1982 认为先后关系“未定义”，
// 采集站此时必须停止切换并说明原因。

// SerialHalf 是序列号空间的一半：2^31。
const SerialHalf = uint32(1) << 31

// SerialRelation 表示两个序列号按 RFC 1982 比较得到的关系。
type SerialRelation int

const (
	// SerialLess 表示第一个序列号早于第二个。
	SerialLess SerialRelation = iota
	// SerialEqual 表示两个序列号相同。
	SerialEqual
	// SerialGreater 表示第一个序列号晚于第二个。
	SerialGreater
	// SerialUncertain 表示差值恰为 2^31，先后关系无法确定。
	SerialUncertain
)

// CompareSerial 按 RFC 1982 的 32 位规则比较 a 与 b。
func CompareSerial(a, b uint32) SerialRelation {
	if a == b {
		return SerialEqual
	}
	// 无符号减法天然按 2^32 取模。
	d := b - a
	switch {
	case d < SerialHalf:
		return SerialLess
	case d > SerialHalf:
		return SerialGreater
	default:
		// d == 2^31：RFC 1982 第 3.2 节，此时关系未定义。
		return SerialUncertain
	}
}

// String 返回关系的机器可读名称。
func (r SerialRelation) String() string {
	switch r {
	case SerialLess:
		return "less"
	case SerialEqual:
		return "equal"
	case SerialGreater:
		return "greater"
	default:
		return "uncertain"
	}
}

// DescribeSerial 返回关系的中文说明，用于网页展示与拒收原因。
func DescribeSerial(rel SerialRelation, oldSerial, newSerial uint32) string {
	switch rel {
	case SerialLess:
		return "新区序列号更新（按 RFC 1982 比较，允许切换）"
	case SerialEqual:
		return "序列号相同"
	case SerialGreater:
		return "新区序列号比本地旧（按 RFC 1982 比较）"
	default:
		return "序列号差值恰为 2^31，按 RFC 1982 无法确定新旧，已停止切换"
	}
}
