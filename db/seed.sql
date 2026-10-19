-- 种子数据：可重复执行（固定主键 + ON DUPLICATE KEY UPDATE，重复跑不报错、行数不变）。
-- 密码字段是占位符、不是真实哈希，仅用于结构演示；真实登录在 Day 18+ 才接。
USE blog_my;

INSERT INTO `user` (`id`, `username`, `password`, `created_at`) VALUES
  ('00000000-0000-0000-0000-000000000001', 'demo',  '$2a$10$PLACEHOLDER_HASH_NOT_REAL', NOW(3)),
  ('00000000-0000-0000-0000-000000000002', 'alice', '$2a$10$PLACEHOLDER_HASH_NOT_REAL', NOW(3)),
  ('00000000-0000-0000-0000-000000000003', 'bob',   '$2a$10$PLACEHOLDER_HASH_NOT_REAL', NOW(3))
ON DUPLICATE KEY UPDATE username = VALUES(username);

INSERT INTO `conversation` (`id`, `user_id`, `title`, `model`, `created_at`, `updated_at`) VALUES
  (1, '00000000-0000-0000-0000-000000000001', '帮我写一段自我介绍', 'deepseek-chat', NOW(3), NOW(3)),
  (2, '00000000-0000-0000-0000-000000000001', '把这段话翻译成英文，语气正式一点', 'gpt-4o-mini', NOW(3), NOW(3)),
  (3, '00000000-0000-0000-0000-000000000001', '用生活例子解释递归', 'deepseek-chat', NOW(3), NOW(3)),
  (4, '00000000-0000-0000-0000-000000000002', '给朋友写一条生日祝福', 'gpt-4o-mini', NOW(3), NOW(3)),
  (5, '00000000-0000-0000-0000-000000000002', '周报的标题起个草稿', 'deepseek-chat', NOW(3), NOW(3)),
  (6, '00000000-0000-0000-0000-000000000003', '推荐三首适合练听力的英文歌', 'gpt-4o-mini', NOW(3), NOW(3))
ON DUPLICATE KEY UPDATE title = VALUES(title);

INSERT INTO `message` (`id`, `conversation_id`, `role`, `content`, `created_at`) VALUES
  (1, 1, 'user', '帮我写一段 30 秒的自我介绍，面试用。', NOW(3)),
  (2, 1, 'assistant', '面试官您好，我叫王小明，擅长把复杂问题拆成小步骤逐个解决。过去一年我完成了一个多模型对话站的小项目。谢谢！', NOW(3)),
  (3, 2, 'user', '把这段话翻译成英文，语气正式一点：我们下周开会讨论方案。', NOW(3)),
  (4, 2, 'assistant', 'Here is a formal version: We would like to schedule a meeting next week to discuss the proposal.', NOW(3)),
  (5, 3, 'user', '用生活例子解释一下什么是递归。', NOW(3)),
  (6, 3, 'assistant', '想象你站在镜子前，镜子里还有一面镜子，一层套一层——这就是最直观的“自己调用自己”。', NOW(3)),
  (7, 4, 'user', '给好朋友写一条生日祝福，要真诚不油腻。', NOW(3)),
  (8, 4, 'assistant', '生日快乐！认识你这么多年，最幸运的就是有事情总能第一个想到你。', NOW(3)),
  (9, 5, 'user', '帮我的周报标题起个草稿，本周主要在修前端样式。', NOW(3)),
  (10, 5, 'assistant', '给你三个方向：1. 前端体验修复周记 2. 界面问题修复 3. 给页面做体检和补牙。', NOW(3)),
  (11, 6, 'user', '推荐三首适合练英语听力的歌，慢一点的。', NOW(3)),
  (12, 6, 'assistant', '1. Yesterday Once More 2. Photograph 3. City of Stars。', NOW(3))
ON DUPLICATE KEY UPDATE content = VALUES(content);

INSERT INTO `user_key` (`id`, `user_id`, `endpoint`, `api_key`, `created_at`, `updated_at`) VALUES
  (1, '00000000-0000-0000-0000-000000000001', 'deepseek', 'ENC:PLACEHOLDER_BASE64', NOW(3), NOW(3)),
  (2, '00000000-0000-0000-0000-000000000002', 'openai', 'ENC:PLACEHOLDER_BASE64', NOW(3), NOW(3))
ON DUPLICATE KEY UPDATE endpoint = VALUES(endpoint);
