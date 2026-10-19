# SQL 参考

这里的 schema 文件保存早期 SQL 建表参考，可能不包含后续的用户画像、记忆动力学、助手及团队字段。当前运行模型以 `internal/model/` 为准，启动由 `internal/model/db.go` 执行 GORM AutoMigrate 与兼容迁移。

不要用旧 schema 覆盖已有数据库。目录整理不会迁移、导出或删除 MySQL 用户数据。集成测试应显式指定专用 DB_NAME。
