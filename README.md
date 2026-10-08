# columnar-encoding-engine-go

一个列式编码与压缩引擎：把大规模结构化数据按块（block）编码落盘，
支持有限内存下的流式写入与随机读取。落盘格式带版本号，可长期可读，
不同版本的编码可以共存。

包路径：`github.com/Diudiuseven/columnar-encoding-engine-go/columnar`

## 快速上手

```go
schema := &columnar.Schema{Columns: []columnar.Column{
    {Name: "id", Type: columnar.Int64Type},
    {Name: "name", Type: columnar.StringType},
    {Name: "score", Type: columnar.Float64Type},
}}

// 写入（流式，内存占用约为一个块）
w, _ := columnar.NewWriter("data.ceng", schema, nil) // nil = 默认配置
w.WriteBatch(batch)                                  // 任意大小的批次
w.Close()                                            // 落 footer 并原子发布

// 读取（投影 + 谓词下推，可并发）
r, _ := columnar.Open("data.ceng")
defer r.Close()
s, _ := r.Scan([]string{"name"}, []columnar.Predicate{
    {Column: "id", Op: columnar.Ge, Value: int64(1000)},
})
for s.Next() {
    b := s.Batch() // 只含投影列、只含匹配行
}
```

## 文件格式布局

所有整数小端。校验一律为 CRC-32C（Castagnoli）。

```
[文件头]  magic "CENG"(4)  formatVersion(2)
[块 0][块 1] ... [块 N]
[footer][footerLen(4)][magic "GNE1"(4)]
```

**块布局**（块是行组，每列独立编码为一个 chunk）：

```
numRows(4) numCols(2)
每列: encodingID(1) encodingVersion(1) chunkLen(4)
headerCRC(4)                       -- 覆盖以上头部字节
每列 chunk: payload + crc32c(4)    -- 每个 chunk 独立校验
```

**chunk payload**：

```
nullBitmapLen(4) nullBitmap valuePayload
```

空值位图每行 1 bit（LSB 优先），无空值时长度为 0。valuePayload 只含
非空值，按行序排列。因此空值位置与重复值都能精确还原。

**footer**：schema（列名 + 类型）、每个块的偏移与行数、每列每块的
统计信息（min、max、nullCount），最后 4 字节 CRC。读取端先读尾部
footerLen 与 magic，再解析 footer，即可随机定位任意块。

## 编码方式与选择策略

所有编码无损。字符串列使用专用的 plain/dict 编码；float64 列按
IEEE-754 位模式复用 int64 编码族。

| ID | 编码 | 负载格式 |
|----|------|----------|
| 1  | PLAIN   | 定长 8 字节 / 长度前缀字符串 |
| 2  | BITPACK | min(8) + 位宽(1) + 位打包的 (v-min) |
| 3  | RLE     | [value(8) count(4)]* |
| 4  | DICT    | 字典（首次出现序）+ 位打包 id |
| 5  | DELTA   | first(8) + delta(8)，仅精确等差数列 |

写入时按**每列每块**的数据特征自动选择，判定顺序固定，因此选择结果
是数据的确定函数（无法判定时兜底为 PLAIN）：

1. 全部相同 → RLE
2. 长度 ≥ 3 的精确等差数列 → DELTA
3. 行程数 × 2 ≤ 行数 → RLE
4. 去重数 ≤ 256 且 × 2 ≤ 行数 → DICT
5. 值域位宽 ≤ 48 bit → BITPACK
6. 否则 → PLAIN（确定的兜底）

字符串：去重数 ≤ 4096 且 × 2 ≤ 行数 → DICT，否则 PLAIN。

## 统计信息与谓词跳过

每列每块记录 `min / max / nullCount / numRows`。读取时先用统计信息
做块级剪枝（`Eq/Ne/Lt/Le/Gt/Ge/IsNull/NotNull`），被跳过的块绝不
读取；剪枝规则保证不会误跳含匹配行的块：

- 空值只匹配 `IsNull`；任何值比较对空值都为假；
- 全空块上除 `IsNull` 外的谓词都不匹配；
- `Ne v` 仅在所有非空值都等于 `v` 时可跳过；
- float 采用全序比较（NaN 视为最小且 NaN == NaN），保证统计与
  谓词语义一致。

通过剪枝的块内再做行级过滤。只有投影列与谓词列会被读取和解码，
任何列都不会被整体载入内存（内存上界约为单块所需列的大小）。

## 版本演进

- 文件头含 `formatVersion`，高于本库支持版本时返回
  `ErrUnsupportedVersion`；
- 每个 chunk 独立记录 `encodingID + encodingVersion`，遇到未知编码
  返回 `ErrUnknownEncoding`，遇到更高编码版本返回
  `ErrUnsupportedVersion`——明确报错，绝不崩溃或静默读错；
- 旧版本文件在新版本读取端可正常打开（解码按版本分派，旧路径保留）。

## 并发与原子性

- 写入：同一路径同一时刻只允许一个 Writer（对 `<path>.tmp` 使用
  `O_EXCL` 创建，冲突返回 `ErrWriterExists`）；`Close` 时先写完整
  临时文件再原子 rename 为目标路径，读取者永远看不到半成品；
- 读取：`Reader`/`Scanner` 基于 `ReadAt`，无共享可变状态，多个
  读取者可并发读同一文件（`-race` 测试覆盖）。

## 损坏检测

头部 magic、footer CRC、块头 CRC、每个 chunk 的 CRC 层层校验：

- 字节被篡改 → `ErrCorrupt`（可用 `errors.Is` 区分）；
- 输入被截断 → `ErrTruncated`；
- 所有解析路径都有边界检查，任何损坏输入都不会触发 panic。

## 确定性

相同输入 + 相同配置 → 输出逐字节一致：字典按首次出现序、无时间戳、
无随机性、无 map 迭代序依赖。测试见 `TestDeterministicOutput`。

## 配置项

`columnar.Config`：

| 字段 | 默认 | 说明 |
|------|------|------|
| `BlockRows` | 8192 | 每块目标行数；同时决定写内存上界与读跳过粒度 |

## 测试

```
go test ./...        # 单元 + 集成
go test -race ./...  # 并发读写竞态
```

覆盖：空值、极端取值（MinInt64/MaxInt64/NaN/±Inf/空串/超长串）、
编码选择边界、逐字节可复现、截断与篡改输入、未知编码/版本、
多读者并发、单写者互斥与原子发布、十万行级流式读写。
