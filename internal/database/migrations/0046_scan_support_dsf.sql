-- +goose Up
-- 将 dsf 加入已有部署的 scan_config.supported_formats，使 DSD（DSD Stream
-- File）能被扫描到（B11：RAAT DoP 路径的输入侧；0001_init.sql 的种子值
-- 遗漏了 dsf——TestScanAndImportDSFWithDefaultConfig 在既有部署上失败的
-- 根因）。
-- 幂等：仅当数组中尚无 "dsf" 时追加。
UPDATE configs
SET value = json_set(value, '$.supported_formats[#]', 'dsf')
WHERE key = 'scan_config'
  AND value LIKE '%"supported_formats"%'
  AND value NOT LIKE '%"dsf"%';

-- +goose Down
UPDATE configs
SET value = json_set(value, '$.supported_formats',
  (SELECT json_group_array(e.value)
     FROM json_each(json_extract(configs.value, '$.supported_formats')) e
    WHERE e.value <> 'dsf'))
WHERE key = 'scan_config' AND value LIKE '%"dsf"%';
