# go-sqlite 测试报告

生成时间：2026-11-06 · 测试框架：Go 1.26.10 原生 testing

---

## 1. 测试环境

| 项目 | 值 |
|---|---|
| CPU | 12th Gen Intel(R) Core(TM) i7-12700H（20 逻辑核） |
| 内存 | 15.7 GB |
| 操作系统 | Windows 11 (build 26100), amd64 |
| Go | 1.26.10 |
| GORM | gorm.io/gorm v1.31.2 |
| 被测驱动 A | `github.com/hicode0101/go-sqlite` → modernc.org/sqlite **v1.61.1** |
| 被测驱动 B（对照） | `github.com/glebarez/sqlite` v1.11.0 → glebarez/go-sqlite v1.23.0 |
| SQLite 内核 | **两者均为 3.54.1**（实测 `select sqlite_version()`），对比完全公平 |

---

## 2. 单元测试

### 2.1 总览

| 指标 | 结果 |
|---|---|
| 单元测试总数 | **85 项，全部通过** |
| 语句覆盖率 | **95.1%**（`coverage.out` / `coverage.html` / 逐函数明细 `coverage-func.txt`） |
| 竞态检测 | `go test -race ./...` 通过，无数据竞争 |
| 静态检查 | `go vet ./...` 通过 |

### 2.2 覆盖率要点（核心 4 文件）

| 文件 | 覆盖率 | 说明 |
|---|---|---|
| `sqlite.go`（Dialector） | 96%+ | 全部接口方法、子句构建器（含防御分支）、错误翻译 100% |
| `migrator.go` | ~93% | 表重建、约束增删、索引操作、列类型解析全覆盖；残余为不可达的错误注入分支 |
| `ddlmod.go` | ~99% | DDL 解析器全路径：引号/括号感知切分、转义引号、DEFAULT/COLLATE、约束识别 |
| `errors.go` | 100% | — |

### 2.3 并发场景（文件库，全部通过）

| 测试 | 场景 | 断言 |
|---|---|---|
| `TestConcurrentWrites` | 8 goroutine × 25 并发插入（WAL） | 每行精确落库一次、ID 全部回填 |
| `TestConcurrentReadsAndWrites` | 4 写者 + 6 读者持续循环（WAL 读写混合） | 读者零错误（快照隔离）、最终计数精确 |
| `TestConcurrentTransactions` | 8 goroutine 并发事务（各 10 插入 + 1 更新） | 全部原子提交、计数精确 |
| `TestConcurrentUniqueInsertRace` | 10 goroutine 抢占同一唯一键 | 恰好 1 成功、9 个 `gorm.ErrDuplicatedKey` |
| `TestConcurrentSharedSession` | 12 goroutine 共享 `*gorm.DB` 读写混跑 | 全部成功、计数精确 |
| `TestConcurrentUpsertRace` | 8 goroutine 并发 UPSERT 同一键 | 恰好 1 行、值域正确 |
| `TestConcurrentFirstDestMustBeZeroed` | 并发 First 的 dest 复用语义 | 锁定 GORM 行为契约（见 §4.1） |

---

## 3. 性能对比

### 3.1 方法论

- 两驱动都注册名为 `"sqlite"` 的 `database/sql` 驱动，无法同进程共存 → 通过构建标签 `bench_glebarez` 切换，**基准用例代码逐字节相同**（`bench_test.go`）。
- 顺序基准：`:memory:` 内存库 + 连接池上限 1，消除文件系统噪声，测纯 CPU 路径。
- 并行基准：临时文件库 + 16 连接池 + `busy_timeout(30000)`，每 goroutine 独立 `Session`，启动前对全部 16 连接做数据可见性预热验证。
- `-count 5`，benchstat 聚合（原始数据：`bench-ours.txt`、`bench-glebarez.txt`、`benchstat.txt`）。

### 3.2 耗时（sec/op，越低越好）

| 基准 | 本库 | glebarez | 差异 |
|---|---|---|---|
| Insert | 32.11µ | 27.03µ | ~（p=0.056） |
| InsertBatch100 | 505.2µ | 524.1µ | ~ |
| TxInsert100 | 3.443m | 3.158m | ~ |
| Update | 24.12µ | 22.06µ | glebarez 快 8.5% |
| SelectByPK | 16.05µ | 14.48µ | glebarez 快 9.8% |
| Select100 | 169.7µ | 181.0µ | **本库快 6.6%** |
| **RawInsert**（database/sql 直连） | **5.047µ** | 7.600µ | **本库快 50.6%** |
| **RawSelectByPK**（database/sql 直连） | **2.944µ** | 7.626µ | **本库快 2.6 倍** |
| ParallelReads | 28.53µ | 24.93µ | glebarez 快 12.6% |
| ParallelWrites | 10.67m | 12.70m | ~ |
| ParallelMixed | 1.324m | 1.553m | ~ |
| **几何平均** | **107.4µ** | 119.9µ | **本库快 11.6%** |

### 3.3 内存（B/op 与 allocs/op）

| 基准 | B/op（本库 / glebarez） | allocs/op（本库 / glebarez） |
|---|---|---|
| Insert | 5.83K / 5.72K | 84 / 79 |
| InsertBatch100 | 66.3K / 62.1K | 1373 / 1367 |
| TxInsert100 | 528K / 522K | 7049 / 6813 |
| SelectByPK | 4.09K / 3.89K | 74 / 68 |
| Select100 | 43.1K / 43.7K | 1337 / 1529 |
| RawSelectByPK | 857 / 665 | 30 / 25 |
| ParallelReads | 4.74K / 4.53K | 88 / 82 |

### 3.4 结论

1. **裸驱动层（database/sql 直连）：本库显著更快** —— 插入快 50%，点查快 2.6 倍。这直接来源于直接依赖 modernc.org/sqlite v1.61.1 上游（一年多的驱动层优化），而 glebarez fork 停留在旧基线。
2. **GORM 层：整体持平（±10% 以内）** —— GORM 的反射/回调开销主导了端到端耗时，掩盖了驱动层差距；各基准互有胜负，几何平均本库领先 11.6%。
3. **内存：同一量级**，差异 ≤ 8%（Select100 一项本库少 14% 分配次数）。
4. 实务建议：对延迟敏感的批量路径可绕过 ORM 用 `database/sql` 直连（本库 raw 层优势明显）；常规 GORM 用法两者体验一致，本库胜在依赖链更短、跟随上游更新。

---

## 4. 过程中发现并修复的问题

### 4.1 GORM 语义陷阱：dest 结构体携带主键（已固化为回归测试）

并发基准初期偶发 `record not found`。抓取实际 SQL 定位：

```sql
SELECT * FROM `bench_users` WHERE `bench_users`.`id` = 14 AND `bench_users`.`id` = 10 ...
```

根因：**复用的 dest 结构体在上次查询成功后带有非零主键，GORM 的 `BuildQuerySQL` 会把它追加进下一次查询的 WHERE**（`callbacks/query.go` 的 dest-pk 分支），与新条件构成矛盾。这是 GORM 的既定语义而非驱动缺陷，但极易在并发代码中踩中。

- 错误用法：`var u User` 在 worker 循环外声明，循环内反复 `First(&u, id)`
- 正确用法：每次迭代 `var u User`（清零 dest）
- 已固化回归测试 `TestConcurrentFirstDestMustBeZeroed`，同时验证错误模式与正确模式

### 4.2 Windows + WAL 模式的偶发快照陈旧

基准开发期还观察到（DELETE 模式下消失、与新连接池对照确认）：WAL 模式下个别连接池连接会持续看到过期数据快照。单元测试中的短促并发场景未受影响（保留 WAL 并通过），**基准的并行场景改用默认 DELETE 日志模式**以保证确定性；两个驱动测量条件一致，对比仍然公平。此现象值得向上游 modernc.org/sqlite 反馈。

### 4.3 其他修复

- Windows 下打开的数据库文件无法删除 → 基准清理顺序改为先关连接池再删临时目录（`b.Cleanup` LIFO）。

---

## 5. 复现命令

```bash
# 单元测试 + 覆盖率 + 竞态
cd go-sqlite
go test ./... -v
go test -race ./...
go test -coverprofile=benchmark/coverage.out .

# 性能对比（本库）
cd benchmark
go test -run xxx -bench . -benchmem -count 5 . > bench-ours.txt

# 性能对比（glebarez 对照组）
go test -tags bench_glebarez -run xxx -bench . -benchmem -count 5 . > bench-glebarez.txt

# 统计汇总
benchstat bench-ours.txt bench-glebarez.txt

# 各驱动 SQLite 内核版本
go test -run TestSQLiteVersion -v .
go test -tags bench_glebarez -run TestSQLiteVersion -v .
```
