package i18n

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"zh":      "zh",
		"zh-CN":   "zh",
		"zh-TW":   "zh",
		"zh-Hans": "zh",
		"ja":      "ja",
		"ja-JP":   "ja",
		"en":      "en",
		"en-US":   "en",
		"en_GB":   "en",
		"fr":      "",
		"":        "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh"},
		{"ja-JP,ja;q=0.9,en;q=0.8", "ja"},
		{"en-US,en;q=0.9", "en"},
		{"en;q=0.5,zh;q=0.9", "zh"}, // q 值优先
		{"fr-FR,de-DE", "en"},       // 全部不支持 → 兜底英语
		{"", "en"},
	}
	for _, tc := range cases {
		if got := FromAcceptLanguage(tc.header); got != tc.want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		pref, header, want string
	}{
		{"auto", "ja-JP,ja;q=0.9,en;q=0.8", "ja"},
		{"auto", "en-US,en;q=0.9", "en"},
		{"auto", "fr-FR", "en"},
		{"zh", "en-US,en;q=0.9", "zh"}, // 显式偏好优先于浏览器
		{"ja", "en-US", "ja"},
		{"en", "zh-CN,zh;q=0.9", "en"},
		{"", "zh-CN", "zh"},
		{"fr", "zh-CN", "zh"}, // 非法偏好按 auto 处理
	}
	for _, tc := range cases {
		if got := Resolve(tc.pref, tc.header); got != tc.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", tc.pref, tc.header, got, tc.want)
		}
	}
}

func TestT(t *testing.T) {
	key := "收件箱"
	// zh 恒等于 key 本身
	if got := T("zh", key); got != key {
		t.Errorf("T(zh) = %q, want %q", got, key)
	}
	// en
	if got, want := T("en", key), "Inbox"; got != want {
		t.Errorf("T(en) = %q, want %q", got, want)
	}
	// ja
	if got, want := T("ja", key), "受信トレイ"; got != want {
		t.Errorf("T(ja) = %q, want %q", got, want)
	}
	// 未知语言 → 英文兜底
	if got := T("fr", key); got != "Inbox" {
		t.Errorf("T(fr) = %q, want Inbox", got)
	}
	// 无此键 → 原样返回 key
	if got := T("en", "不存在的键"); got != "不存在的键" {
		t.Errorf("T(missing) = %q, want key itself", got)
	}
}

func TestTF(t *testing.T) {
	key := "用户名或密码错误，还剩 %d 次尝试机会"
	if got, want := TF("zh", key, 3), "用户名或密码错误，还剩 3 次尝试机会"; got != want {
		t.Errorf("TF(zh) = %q, want %q", got, want)
	}
	if got, want := TF("en", key, 2), "Incorrect username or password. 2 attempts remaining"; got != want {
		t.Errorf("TF(en) = %q, want %q", got, want)
	}
	if got, want := TF("ja", key, 1), "ユーザー名またはパスワードが違います。あと 1 回試行できます"; got != want {
		t.Errorf("TF(ja) = %q, want %q", got, want)
	}
}

// TestCatalogCoverage 校验 en/ja 目录中不包含空译文。
func TestCatalogCoverage(t *testing.T) {
	for k, v := range en {
		if v == "" {
			t.Errorf("en 目录空译文: %q", k)
		}
	}
	for k, v := range ja {
		if v == "" {
			t.Errorf("ja 目录空译文: %q", k)
		}
	}
}
