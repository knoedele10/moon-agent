-- blog-my 数据库结构（与 model/data.go 的 GORM 定义严格一致）
-- 用法：mysql -u root -p blog_my < db/schema.sql   （或直接让 GORM AutoMigrate）
-- 每张表字段类型的选择原因，见 db/schema.md 与今日复盘。

CREATE DATABASE IF NOT EXISTS blog_my DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE blog_my;

-- 用户表：站点账号。密码只存 bcrypt 哈希，绝不存明文。
CREATE TABLE IF NOT EXISTS `user` (
  `id`         VARCHAR(36)  NOT NULL COMMENT '主键：UUID 字符串（36 位），由应用层生成，不用自增数字便于日后分库',
  `username`   VARCHAR(64)  NOT NULL COMMENT '用户名：唯一；64 足够日常用户名',
  `password`   VARCHAR(100) NOT NULL COMMENT 'bcrypt 哈希：固定 60 字符，留 100 冗余',
  `created_at` DATETIME(3)  NULL COMMENT '创建时间：毫秒精度，GORM time.Time 的默认映射',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_user_username` (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站点用户';

-- 用户密钥表：用户为某个端点填的 API Key，存 AES-GCM 密文。
CREATE TABLE IF NOT EXISTS `user_key` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：自增，密钥是附属数据、量级大用数字 id 更省空间',
  `user_id`    VARCHAR(36)  NOT NULL COMMENT '外键：关联 user.id，表示这个 key 属于谁',
  `endpoint`   VARCHAR(64)  NOT NULL COMMENT '端点标识（如 deepseek / openai），用于区分厂商',
  `api_key`    TEXT         NOT NULL COMMENT 'AES-GCM 密文（base64），可长可短，用 TEXT 更稳',
  `created_at` DATETIME(3)  NULL,
  `updated_at` DATETIME(3)  NULL,
  PRIMARY KEY (`id`),
  KEY `idx_user_key_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户的 API Key（密文）';

-- 会话表：一次多轮对话的元信息。
CREATE TABLE IF NOT EXISTS `conversation` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：自增',
  `user_id`    VARCHAR(36)  NOT NULL COMMENT '外键：关联 user.id',
  `title`      VARCHAR(255) NULL COMMENT '会话标题，默认取首条用户消息前若干字',
  `model`      VARCHAR(128) NULL COMMENT '本次会话使用的模型 id',
  `created_at` DATETIME(3)  NULL,
  `updated_at` DATETIME(3)  NULL COMMENT '更新于每次新消息，用于按最近活跃排序',
  PRIMARY KEY (`id`),
  KEY `idx_conversation_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话';

-- 消息表：会话里的每一句话。
CREATE TABLE IF NOT EXISTS `message` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：自增',
  `conversation_id` BIGINT UNSIGNED NOT NULL COMMENT '外键：关联 conversation.id，把消息挂到会话下',
  `role`            VARCHAR(16)  NOT NULL COMMENT '角色：system / user / assistant',
  `content`         LONGTEXT     NULL COMMENT '正文：AI 回复可能很长，用 LONGTEXT 容纳',
  `created_at`      DATETIME(3)  NULL,
  PRIMARY KEY (`id`),
  KEY `idx_message_conversation_id` (`conversation_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话消息';
