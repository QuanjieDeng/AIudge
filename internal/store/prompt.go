package store

// SystemPrompt 返回自定义的审核 prompt。空串表示用内置默认——这样升级二进制时
// 默认 prompt 的改进能自动生效，只有真的自定义过才需要人维护。
func (d *DB) SystemPrompt() (string, error) {
	var s string
	err := d.QueryRow(`SELECT system_prompt FROM review_configs WHERE id=1`).Scan(&s)
	return s, err
}

func (d *DB) UpdateSystemPrompt(prompt string) error {
	_, err := d.Exec(`UPDATE review_configs SET system_prompt=?, updated_at=? WHERE id=1`, prompt, Now())
	return err
}
