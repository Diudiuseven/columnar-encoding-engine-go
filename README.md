# columnar-encoding-engine-go

一个 Go 实现的列式编码与压缩引擎：把大规模结构化数据按行块（block）切分，
每块内每列独立编码、可选压缩后落盘，支持有限内存下的流式写入与随机读取。

## 特性

- **多种列编码**：字典编码（dict）、行程编码（rle）、位打包（bitpack）、
  等差编码（arith）、明文（plain，可确定的兜底）。
- **自动编码选择**：按每列每块的数据特征选择最小编码，平局按固定优先级，
  结果完全可确定。
- **无损**：解码结果与原始值逐条一致，空值位置与重复值精确保留；
  支持 int64 / float64 / string / bool 列。
- **块级统计**：每块每列记录 min / max / 空值计数 / 行数，读取时做谓词跳过，
  被跳过的块保证不含匹配行（空值语义正确）。
- **随机访问与投影**：只解码命中的块与请求的列，任何列都不会被整体载入内存。
- **格式可演进**：文件头带格式版本，每个块带编码 id 与编码版本；
  遇到未知编码或更高版本返回明确错误，绝不崩溃或静默读错。
- **并发安全**：任意多个读取者可并发读同一文件；同时只允许一个写入者；
  写入通过临时文件 + 原子 rename 发布，读取者永远看不到半成品。
- **损坏检测**：每个块与目录均有 CRC-32 校验；截断、篡改、越界都返回
  可区分的错误，任何输入都不会触发 panic。
- **逐字节可复现**：相同输入 + 相同配置产生完全相同的输出字节。

## 文件格式布局

```
header:  magic "CENR"(4) | major(1) | minor(1) | reserved(2)

blocks:  连续的 chunk 序列。每个 chunk 是某一块中某一列的编码数据：
           uvarint nullBitmapLen | null bitmap | 编码后的非空值
         chunk 可经 flate 压缩（目录中标记）。位图第 i 位置 1 表示第 i 行为空。

footer:  directory | dirLen u32 LE | dirCRC32 u32 LE | magic "CENR"(4)

directory（确定性二进制序列化）:
  uvarint 列数; 每列: uvarint 名长 | 名 | u8 类型
  uvarint 块数; 每块: uvarint 行数; 每列 chunk 元数据:
    u8 编码 id | u8 编码版本 | u8 压缩方式
    uvarint 文件偏移 | uvarint 落盘长度 | uvarint 解压前长度
    u32 CRC-32(IEEE, 对落盘字节)
    统计: uvarint 行数 | uvarint 空值数 | u8 是否有 min/max | [min | max]
```

所有多字节整数为小端或 uvarint；不写入时间戳、不序列化 map，
因此输出对相同输入逐字节可复现。

### 编码布局（encoding version = 1）

| 编码 | id | 适用类型 | 布局 |
|------|----|----------|------|
| plain | 1 | 全部 | 值顺序拼接：int64/float64 8 字节 LE，bool 1 字节，string 为 uvarint 长度 + 字节 |
| dict  | 2 | 全部 | uvarint 字典长 D，D 个 plain 值（按首次出现序），uvarint 索引位宽 W，ceil(n·W/8) 字节 LSB-first 索引 |
| rle   | 3 | 全部 | 重复段序列：plain 值 + uvarint 行程长度 |
| bitpack | 4 | int64 | base(8B LE) + uvarint 位宽 + ceil(n·W/8) 字节 LSB-first 的 (value−base) |
| arith | 5 | int64 | start(8B LE) + step(8B LE)，第 i 值为 start+i·step（mod 2^64 环绕） |

## 编码选择策略

对每个块的非空值，逐一尝试所有适用编码，取编码后字节数最小者；
平局按固定优先级 `arith < rle < dict < bitpack < plain` 裁定。
`plain` 永远适用，是无法判断时的确定性兜底。
`arith` 仅在序列严格满足等差时适用；`bitpack`/`arith` 仅适用整数列。

## 配置项

```go
type Options struct {
    // 每块缓冲的行数，达到即编码落盘。默认 8192。
    BlockRows int
    // 块压缩方式：CompNone（默认）或 CompFlate。
    // 仅在压缩后更小时采用，选择本身也是确定性的。
    Compression Compression
}
```

## 使用示例

```go
schema := columnar.Schema{Columns: []columnar.Column{
    {Name: "id", Type: columnar.KindInt64},
    {Name: "name", Type: columnar.KindString},
}}

// 写入（流式，内存占用以块为界）
w, _ := columnar.NewWriter("data.cen", schema, &columnar.Options{
    BlockRows:   8192,
    Compression: columnar.CompFlate,
})
w.WriteRow(columnar.Int64(1), columnar.String("alice"))
w.WriteRow(columnar.Int64(2), columnar.NullOf(columnar.KindString))
w.Close() // 原子发布；中途失败可 w.Abort()

// 读取（可并发，投影 + 谓词跳过）
r, _ := columnar.Open("data.cen")
defer r.Close()
blocks, _ := r.MayMatch(columnar.Predicate{
    Column: "id", Op: columnar.OpEq, Val: columnar.Int64(2),
})
for _, b := range blocks {
    cols, _ := r.ReadBlock(b, []int{0, 1}) // 只解码需要的列与块
    _ = cols
}
```

## 错误约定

用 `errors.Is` 区分错误类别：

- `ErrUnsupportedVersion`：文件格式主版本或编码版本高于本库支持范围。
- `ErrUnsupportedEncoding`：未知的编码 id 或压缩方式。
- `ErrCorrupt`：magic 错误、CRC 不匹配、截断、长度不一致等损坏。
- `ErrSchema`：写入值与 schema 不符、谓词列不存在等。
- `ErrWriterExists`：同一路径已有活跃写入者。

## 测试

```sh
go test ./...        # 单元 + 端到端 + 损坏注入 + 并发
go test -race ./...  # 竞态检测
```

覆盖：空值、极端取值（MinInt64/MaxInt64/NaN/±Inf/空字符串）、
各编码边界（位宽 0/64、单元素、空块）、损坏输入（截断/篡改/随机变异不 panic）、
并发读写、单写入者互斥、输出逐字节可复现（golden 测试）。
