-- Day 17 新增两张表：hot（热搜）与 favorite（收藏）。是「新增表」，不改动 Day 16 的 conversation/message。
USE blog_my;

CREATE TABLE IF NOT EXISTS `hot` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：自增',
  `platform`   VARCHAR(32)  NOT NULL COMMENT '来源平台：微博 / 抖音 / 知乎 / 百度 等',
  `rank`       INT          NOT NULL COMMENT '平台内排名，1 为最热',
  `title`      VARCHAR(255) NOT NULL COMMENT '热搜标题',
  `heat`       VARCHAR(64)  NULL COMMENT '热度标签（热/新/热议），无则空',
  `fetched_at` DATETIME(3)  NULL COMMENT '抓取时间',
  PRIMARY KEY (`id`),
  KEY `idx_hot_platform_rank` (`platform`, `rank`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='当日热搜（同步自真实数据源）';

CREATE TABLE IF NOT EXISTS `favorite` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键：自增',
  `user_id`    VARCHAR(36)  NULL COMMENT '收藏者，关联 user.id（写入功能 Day 18 接）',
  `title`      VARCHAR(255) NOT NULL COMMENT '收藏内容的标题',
  `url`        VARCHAR(512) NULL COMMENT '原始链接',
  `created_at` DATETIME(3)  NULL,
  PRIMARY KEY (`id`),
  KEY `idx_favorite_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='收藏';
