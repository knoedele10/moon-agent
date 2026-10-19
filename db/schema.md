# db/schema.sql 字段类型选择说明

> 与 `model/data.go` 的 GORM 定义一一对应。这里解释"为什么用这个类型"。

## 主键类型：两种不同的选择
- `conversation` / `message` / `user_key` 用 `BIGINT UNSIGNED AUTO_INCREMENT`：
  这三类是"流水"数据，量会持续增长，用自增数字 id 最省空间、插入最快；`UNSIGNED`（无符号）不浪费符号位，容量翻倍。
- `user` 用 `VARCHAR(36)`（UUID 字符串）：
  用户是"身份"型实体，将来若要分库、合并、跨系统同步，UUID 全局唯一、不用和别的库协调自增值，迁移更稳。

## 字符串长度：按实际最长值留余量
| 字段 | 类型 | 为什么 |
|---|---|---|
| `user.username` | VARCHAR(64) | 用户名一般 ≤ 32，留一倍余量 |
| `user.password` | VARCHAR(100) | bcrypt 哈希固定 60 字符，留 100 |
| `conversation.title` | VARCHAR(255) | 标题取首条消息前若干字，255 足够 |
| `conversation.model` | VARCHAR(128) | 模型 id 是短字符串，128 足够 |
| `message.role` | VARCHAR(16) | 只有 system/user/assistant 三种，16 足够 |
| `message.content` | LONGTEXT | AI 回复可能极长，TEXT(64KB) 不够，LONGTEXT(4GB) 稳 |
| `user_key.api_key` | TEXT | AES-GCM 密文是 base64，可长可短，TEXT 更省心 |

## 时间：DATETIME(3)
GORM 的 `time.Time` 默认映射为 `DATETIME(3)`（毫秒精度）。消息、会话都要按时间排序，毫秒精度能保证同秒内先后不乱。

## 存储引擎与字符集
- `ENGINE=InnoDB`：支持事务和外键，是 MySQL 默认且最常用的引擎。
- `DEFAULT CHARSET=utf8mb4`：能存中文和 emoji（utf8 老别名只覆盖 3 字节，emoji 会报错）。

## 索引（对应 GORM 的 index tag）
- `user_id`（conversation / user_key）：查询"某用户的会话/密钥"要按 user_id 走索引。
- `conversation_id`（message）：查询"某会话的全部消息"要按 conversation_id 走索引。
- `username` 唯一索引：登录按用户名查、且不能重名。

> 注意：外键字段只在逻辑上关联（靠 `message.conversation_id = conversation.id` 这个字段），
> 本 schema 没有建 `FOREIGN KEY` 物理约束——这和 GORM 默认行为一致（只建索引、不建外键约束），
> 好处是写入更灵活、删改不受级联约束卡住。
