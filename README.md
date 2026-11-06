<div align="center">

# go-sqlite

**纯 Go（无 CGO）的 GORM SQLite 方言驱动**

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![GORM](https://img.shields.io/badge/GORM-v1.31-CA003F?logo=go&logoColor=white)](https://gorm.io)
[![SQLite](https://img.shields.io/badge/SQLite-3.54.1-044A64?logo=sqlite&logoColor=white)](https://pkg.go.dev/modernc.org/sqlite)
[![CGO](https://img.shields.io/badge/CGO-free-00B4AB)](#-为什么选择本库)
[![Tests](https://img.shields.io/badge/tests-85%20passing-2EA043)](#-运行测试)
[![Coverage](https://img.shields.io/badge/coverage-95.1%25-2EA043)](#-运行测试)
[![Race](https://img.shields.io/badge/race%20detect-clean-2EA043)](#-运行测试)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

`go get github.com/hicode0101/go-sqlite`

</div>

---

**go-sqlite** 是 [GORM](https://gorm.io) 的 SQLite 方言驱动，直接构建在 modernc.org/sqlite（SQLite 的纯 Go 实现，内嵌 SQLite 3.54.1 内核）之上——不需要安装任何 SQLite 库、不需要 CGO 工具链，`go build` 即可交叉编译到任何 Go 支持的平台。

## 📚 目录

- [架构](#-架构)
- [✨ 为什么选择本库](#-为什么选择本库)
- [📦 安装](#-安装)
- [🚀 快速开始](#-快速开始)
- [⚙️ DSN 与连接参数](#️-dsn-与连接参数)
- [🔌 使用自定义连接池](#-使用自定义连接池)
- [🈯 错误翻译](#-错误翻译)
- [📋 支持的 GORM 特性](#-支持的-gorm-特性)
- [🏁 与 glebarez/sqlite 对比](#-与-glebarezsqlite-对比)
- [🧪 运行测试](#-运行测试)
- [📊 性能对比](#-性能对比)
- [📄 License](#-license)

## 🏗 架构

```
你的代码
   │
   ▼
gorm.io/gorm                       ← ORM 框架
   │
   ▼
github.com/hicode0101/go-sqlite    ← 本库：Dialector + Migrator + 错误翻译
   │
   ▼
modernc.org/sqlite                 ← database/sql 驱动（注册名 "sqlite"）+ 纯 Go SQLite 内核
```

## ✨ 为什么选择本库

- 🚫 **零 CGO** —— 纯 Go 编译，一条 `go build` 交叉编译到任何平台，无需工具链
- 📦 **零中间 fork** —— 直接依赖 `modernc.org/sqlite v1.61.1`，依赖链比 glebarez 短一层，SQLite 版本跟随上游官方发布
- 🔁 **完整迁移支持** —— AutoMigrate 建表、加列、改列、删列、约束、索引，基于标准 SQLite 表重建流程
- 🌐 **`RETURNING` 原生支持** —— SQLite ≥ 3.35 子句默认启用，插入/更新直接回填主键
- 🏷 **哨兵错误翻译** —— `UNIQUE`/`FOREIGN KEY` 冲突翻译成 GORM 哨兵错误，`errors.Is` 即可判断
- 🧵 **并发友好** —— SavePoint / 嵌套事务、Upsert、WAL 并发读写场景均有专项测试
- ✅ **95.1% 覆盖率** —— 85 项测试全部通过，竞态检测干净

## 📦 安装

```bash
go get github.com/hicode0101/go-sqlite
```

要求 **Go ≥ 1.26**（modernc.org/sqlite v1.61.1 的要求）。

> [!WARNING]
> **不要**同时引入其他注册名为 `"sqlite"` 的驱动——`database/sql` 的驱动注册是进程全局的，重复注册同名驱动会 panic。

## 🚀 快速开始

```go
package main

import (
    "log"

    "gorm.io/gorm"
    sqlite "github.com/hicode0101/go-sqlite"
)

type User struct {
    ID    uint   `gorm:"primaryKey;autoIncrement"`
    Name  string `gorm:"size:100"`
    Email string `gorm:"size:100;uniqueIndex"`
}

func main() {
    db, err := gorm.Open(sqlite.Open("app.db"), &gorm.Config{})
    if err != nil {
        log.Fatal(err)
    }

    // 迁移
    if err := db.AutoMigrate(&User{}); err != nil {
        log.Fatal(err)
    }

    // CRUD
    db.Create(&User{Name: "张三", Email: "zhang@example.com"})
    var u User
    db.First(&u, 1)
    db.Model(&u).Update("Name", "李四")
    db.Delete(&u)
}
```

## ⚙️ DSN 与连接参数

底层驱动支持以下数据源名称格式：

| DSN | 说明 |
|---|---|
| `:memory:` | 内存数据库 |
| `file::memory:?cache=shared` | 共享缓存的内存库（多连接可见同一数据） |
| `app.db` | 磁盘文件 |
| `file:app.db?_pragma=foreign_keys(1)` | file URI + 连接参数 |

<details>
<summary><b>常用连接参数（点击展开）</b></summary>

`?` 后跟查询参数，可用 `&` 叠加多个：

```text
_pragma=foreign_keys(1)      # 启用外键约束
_pragma=journal_mode(WAL)    # WAL 日志模式（推荐生产使用）
_pragma=busy_timeout(5000)   # 锁等待超时 5s（推荐）
_pragma=synchronous(NORMAL)  # 同步策略
_txlock=immediate            # 事务默认以 IMMEDIATE 开启（减少写锁竞争）
_time_format=sqlite          # 时间列以 SQLite 文本格式存储
```

生产环境推荐 DSN：

```go
dsn := "file:app.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
```

</details>

> [!TIP]
> SQLite 是单写多读模型，建议限制连接数：

```go
sqlDB, _ := db.DB()
sqlDB.SetMaxOpenConns(1) // 或 1 写 + 多读（见下）
```

## 🔌 使用自定义连接池

```go
import (
    "database/sql"
    "gorm.io/gorm"
    sqlite "github.com/hicode0101/go-sqlite"
)

sqlDB, err := sql.Open("sqlite", "app.db?_pragma=foreign_keys(1)")
db, err := gorm.Open(sqlite.OpenWithConn(sqlDB), &gorm.Config{})
// 或者: gorm.Open(&sqlite.Dialector{Conn: sqlDB})
```

## 🈯 错误翻译

启用 `TranslateError` 后，驱动会把 SQLite 错误码翻译成 GORM 哨兵错误，业务代码可以用 `errors.Is` 判断而无需解析错误字符串：

```go
db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{TranslateError: true})

err = db.Create(&User{Email: "dup@example.com"}).Error
errors.Is(err, gorm.ErrDuplicatedKey)       // UNIQUE / PRIMARY KEY 冲突
errors.Is(err, gorm.ErrForeignKeyViolated)   // 外键约束冲突
```

## 📋 支持的 GORM 特性

| 特性 | 说明 |
|---|---|
| AutoMigrate | 建表、加列、改列、删列、约束、索引——基于标准 SQLite 表重建流程 |
| `RETURNING` 子句 | SQLite ≥ 3.35（本库内嵌 3.54 内核，默认启用） |
| 批量插入主键回填 | `LastInsertIDReversed` 语义 |
| 事务 | SavePoint / RollbackTo（嵌套事务） |
| Upsert | `ON CONFLICT` |
| GORM 通用特性 | 软删除、乐观锁等 |
| `LIMIT` / `OFFSET` | 无 LIMIT 只有 OFFSET 时自动生成 `LIMIT -1` |
| `SELECT ... FOR UPDATE` | 子句静默忽略（SQLite 无行级锁） |

## 🏁 与 glebarez/sqlite 对比

| | glebarez/sqlite | 本库：hicode0101/go-sqlite |
|---|---|---|
| database/sql 驱动 | `glebarez/go-sqlite`（modernc 的 fork，自维护） | **直接依赖 `modernc.org/sqlite v1.61.1`**，无中间 fork |
| 依赖链 | gorm → glebarez/sqlite → glebarez/go-sqlite → modernc/libc | gorm → 本库 → modernc.org/sqlite |
| SQLite 版本跟随 | 跟随 fork 维护者 | 跟随 modernc.org/sqlite 官方发布 |
| API | `sqlite.Open(dsn)` | `sqlite.Open(dsn)` / `sqlite.OpenWithConn(pool)`（兼容用法） |

<details>
<summary><b>为什么可以绕过 fork？（点击展开）</b></summary>

glebarez 当年 fork modernc.org/sqlite 主要是为了 GORM 兼容性（`LastInsertId` 行为、`Error.Code()` 等）；这些能力如今已被 modernc.org/sqlite 上游原生吸收（`result.go` 预取 lastInsertID、`error.go` 提供 `*Error.Code()`、`init()` 注册 `"sqlite"` 驱动、DSN 支持 `_pragma` 参数），因此新库可以绕过 fork 直接依赖上游。

</details>

## 🧪 运行测试

```bash
cd go-sqlite
go test ./... -v          # 全部单元测试（含并发场景）
go test -race ./...       # 竞态检测
go test -cover ./...      # 覆盖率（约 95%）
```

<details>
<summary><b>测试覆盖范围（点击展开）</b></summary>

测试覆盖：Dialector 全部方法（子句构建器、类型映射、错误翻译、标识符转义）、Migrator 全部迁移路径（表重建、约束、索引、列类型解析）、DDL 解析器边界、以及并发场景——

| 并发测试 | 场景 |
|---|---|
| `TestConcurrentWrites` | 8 goroutine × 25 并发插入（WAL），逐行落库 + ID 回填 |
| `TestConcurrentReadsAndWrites` | 4 写者 + 6 读者读写混合（WAL 快照隔离） |
| `TestConcurrentTransactions` | 8 goroutine 并发事务，全部原子提交 |
| `TestConcurrentUniqueInsertRace` | 10 goroutine 抢占同一唯一键，恰好 1 成功 |
| `TestConcurrentSharedSession` | 12 goroutine 共享 `*gorm.DB` 读写混跑 |
| `TestConcurrentUpsertRace` | 8 goroutine 并发 UPSERT 同一键 |

</details>

## 📊 性能对比

`benchmark/` 目录内置与本库同架构的 [glebarez/sqlite](https://github.com/glebarez/sqlite) 的对比基准（两驱动都注册 `"sqlite"` 驱动名，不能同进程共存，故用构建标签切换）：

```bash
cd benchmark
go test -bench . -benchmem -count 5 .                          # 本库
go test -tags bench_glebarez -bench . -benchmem -count 5 .     # glebarez/sqlite
```

**亮点**（完整数据见 [benchmark/REPORT.md](benchmark/REPORT.md)）：

| 基准 | 本库 | glebarez | 差异 |
|---|---|---|---|
| **RawInsert**（database/sql 直连） | **5.05µs** | 7.60µs | **本库快 50.6%** 🚀 |
| **RawSelectByPK**（database/sql 直连） | **2.94µs** | 7.63µs | **本库快 2.6 倍** 🚀 |
| Select100 | 169.7µs | 181.0µs | 本库快 6.6% |
| **几何平均（11 项基准）** | **107.4µs** | 119.9µs | **本库快 11.6%** |

> [!NOTE]
> 裸驱动层优势直接来源于直接依赖 modernc.org/sqlite v1.61.1 上游（一年多的驱动层优化）；GORM 层端到端耗时由 ORM 反射/回调开销主导，两者整体持平（±10% 以内），本库胜在依赖链更短、跟随上游更新。内存占用同一量级（差异 ≤ 8%）。

## 📄 License

[MIT](LICENSE)。SQLite 本身的公有领域声明见 [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)。

<div align="center">

**如果本项目对你有帮助，欢迎 ⭐ Star 支持！**

</div>
