package store

import "aiudge/internal/crypto"

// NotifyConfig 是通知的全局配置。webhook 只回显形态，明文绝不出这层。
type NotifyConfig struct {
	WebhookMask string `json:"webhook_mask"`
	HasWebhook  bool   `json:"has_webhook"`
	SummaryCron string `json:"summary_cron"`
	UpdatedAt   string `json:"updated_at"`
}

func (d *DB) NotifyConfig() (*NotifyConfig, error) {
	var c NotifyConfig
	var enc string
	err := d.QueryRow(`SELECT webhook_enc,summary_cron,updated_at FROM notify_configs WHERE id=1`).
		Scan(&enc, &c.SummaryCron, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.HasWebhook = enc != ""
	if enc != "" {
		if plain, err := crypto.Decrypt(d.key, enc); err == nil {
			c.WebhookMask = maskWebhook(plain)
		} else {
			c.WebhookMask = "****"
		}
	}
	return &c, nil
}

// NotifyWebhook 取全局 webhook 明文，只在发通知和做连通性测试时调用。
func (d *DB) NotifyWebhook() (string, error) {
	var enc string
	if err := d.QueryRow(`SELECT webhook_enc FROM notify_configs WHERE id=1`).Scan(&enc); err != nil {
		return "", err
	}
	if enc == "" {
		return "", nil
	}
	return crypto.Decrypt(d.key, enc)
}

// UpdateNotifyConfig 更新全局通知配置。webhook 传 nil 表示不改（界面上留空即不改），
// 传空串表示清空。
func (d *DB) UpdateNotifyConfig(webhook *string, summaryCron string) error {
	sets := []string{`summary_cron=?`, `updated_at=?`}
	args := []any{summaryCron, Now()}
	if webhook != nil {
		enc, err := d.encryptCredential(webhook)
		if err != nil {
			return err
		}
		sets = append(sets, `webhook_enc=?`)
		args = append(args, enc)
	}
	_, err := d.Exec(`UPDATE notify_configs SET `+joinSets(sets)+` WHERE id=1`, args...)
	return err
}
