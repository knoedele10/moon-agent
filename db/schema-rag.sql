-- blog-my 多模态 RAG 知识库建表脚本（与 model/kb.go 的 GORM 定义严格一致）
-- 目标库：MySQL 5.7（项目环境实测 5.7.19），字符集 utf8mb4。
-- 只「新增」kb_document / kb_chunk 两张表，不改动既有 user / conversation / message / hot / favorite。
-- 用法：mysql -u root -p blog_my < db/schema-rag.sql   （或交给 GORM AutoMigrate）
-- 5.7 限制：全程不用 utf8mb4_0900_ai_ci、CHECK 约束、向量类型、DEFAULT (expr) 等 MySQL 8 才有的特性。
USE blog_my;

-- 知识库文档表：一行 = 上传的一个文本或图片。
-- 原始字节集中存在这里（文本存 UTF-8 字节、图片存原图字节），供展示/溯源/回喂多模态模型；
-- 真正被检索的「文本 + 向量」放在 kb_chunk，两表靠 document_id 逻辑关联。
CREATE TABLE IF NOT EXISTS `kb_document` (
  `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：文档是流水型数据、量持续增长，自增数字 id 比 UUID 省空间、插入更快',
  `user_id`     VARCHAR(36)  NULL COMMENT '归属用户，关联 user.id；空串表示「公共知识库」。可空，对应 GORM 未标 not null（公共库不设归属）',
  `kind`        VARCHAR(16)  NOT NULL COMMENT '文档类型：text | image。入库时按扩展名 + MIME 判定，非空因为每个文档必有类型',
  `title`       VARCHAR(255) NOT NULL COMMENT '标题：上传时取文件名或调用方显式指定。非空保证列表页永远有可显示的名字',
  `source`      VARCHAR(512) NULL COMMENT '原始来源（本地路径或 URL）：浏览器上传时常常没有，故可空；512 足够容纳常见路径',
  `mime`        VARCHAR(64)  NULL COMMENT 'MIME 类型（text/markdown、image/png…）：判定 kind 与后续处理要用，但可从扩展名兜底，故可空',
  `bytes`       BIGINT       NULL COMMENT '原始字节数：冗余存一份，列表展示无需回读 LONGBLOB；BIGINT 对应 Go int64。可空，对应 GORM 未标 not null',
  `sha256`      CHAR(64)     NULL COMMENT '内容指纹(hex)：用于去重。可空是因为流式/早期写入可能暂时没有指纹；CHAR(64) 定长，hex 恰好 64 字符',
  `content`     LONGBLOB     NOT NULL COMMENT '原始内容：文本存 UTF-8 字节，图片存原始图片字节。选 LONGBLOB 而非落磁盘，是为了备份/迁移/审计只碰数据库一份东西',
  `meta`        TEXT         NULL COMMENT '附加信息 JSON（图片宽高、视觉模型名等）：结构会演进，用 TEXT 存 JSON 字符串最省心；文本块通常没有，故可空',
  `chunk_count` BIGINT       NOT NULL DEFAULT 0 COMMENT '切块数量：冗余存储，列表页不必对 kb_chunk 做 COUNT；BIGINT 对应 Go int，默认 0 便于先建文档后补块',
  `created_at`  DATETIME(3)  NULL COMMENT '创建时间：毫秒精度，GORM time.Time 的默认映射',
  PRIMARY KEY (`id`),
  KEY `idx_kb_document_user_id` (`user_id`),
  -- sha256 唯一键：utf8mb4 下 CHAR(64) 占 64*4=256 字节，远小于 InnoDB 767 字节的索引上限，5.7 下安全。
  -- 可空唯一键允许多行 NULL（NULL 不算重复），所以「暂时没有指纹」的文档不会互相冲突。
  UNIQUE KEY `uniq_kb_document_sha256` (`sha256`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='知识库文档（文本或图片，原始字节入库）';

-- 知识库分块表：一行 = 文档切出的一个可检索块。
-- 每块自带一份 1024 维向量；检索时在 Go 里对这些向量做暴力余弦，不依赖数据库的向量能力。
CREATE TABLE IF NOT EXISTS `kb_chunk` (
  `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：块数量远多于文档，数字 id 更省空间',
  `document_id` BIGINT UNSIGNED NOT NULL COMMENT '外键：所属 kb_document.id。只做逻辑关联、不建物理外键（与仓库既有表一致），非空因为块必属于某个文档',
  `seq`         BIGINT       NOT NULL COMMENT '文档内序号，从 0 开始：用于稳定排序、展示「第几块」。BIGINT 对应 Go int',
  `kind`        VARCHAR(16)  NOT NULL COMMENT '块类型：text | image_caption。图片块存的是视觉模型生成的「描述+OCR」，不是原图',
  `text`        MEDIUMTEXT   NOT NULL COMMENT '检索与展示用文本：文本块的正文，或图片块的「描述+OCR」。MEDIUMTEXT(16MB) 能容纳大段正文又比 LONGTEXT 克制；非空因为检索链必须有文本',
  `image_path`  VARCHAR(512) NULL COMMENT '仅图片块用：指向所属文档的标识（便于溯源到原图）；文本块为空，故可空',
  `embedding`   LONGBLOB     NOT NULL COMMENT '1024 维 float32 小端数组 = 4096 字节。用 LONGBLOB 存原始字节，读出后 Go 直接 reinterpret，不引入向量库/向量类型',
  `dim`         BIGINT       NOT NULL COMMENT '向量维度，冗余存储：读向量时先校验 dim=1024 与字节数是否对得上，拦脏数据。BIGINT 对应 Go int',
  `created_at`  DATETIME(3)  NULL COMMENT '创建时间：毫秒精度，GORM time.Time 的默认映射',
  PRIMARY KEY (`id`),
  -- 现按需求只建 document_id 单列索引（与模型定义一致）。
  -- 若日后「按文档取块并按下标排序」成为热点，可再补 `(document_id, seq)` 复合索引，最左前缀仍能覆盖本索引。
  KEY `idx_kb_chunk_document_id` (`document_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='知识库分块（文本 + 向量）';
