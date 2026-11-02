package fingerprint

import "strings"

// tzDisplayNames 时区在 Date#toString 里的显示名（按语种）。
//
// Chrome 的 Date#toString 输出形如：
//
//	Mon Nov 02 2026 18:00:00 GMT+0900 (日本標準時)
//
// 括号里那段由浏览器内核按**系统语言**查 ICU 得到，注入脚本在 JS 层改不到；
// 而它恰恰是检测站「System Time」一行里最扎眼的部分——Intl 说 Asia/Tokyo、
// 括号里却写着中国标准时间，等于自曝「时区是被 JS 改过的」。
//
// 覆盖口径：tzCountry 里的全部时区 × 内置画像用到的语种（en/ja/de/pt/zh）。
// 未覆盖的组合按优先级回退：英文通用名 → 时区 ID 的城市段。
var tzDisplayNames = map[string]map[string]string{
	"America/New_York": {
		"en": "Eastern Standard Time",
		"ja": "東部標準時",
		"de": "Östliche Normalzeit",
		"pt": "Horário Padrão do Leste",
		"zh": "东部标准时间",
	},
	"America/Los_Angeles": {
		"en": "Pacific Standard Time",
		"ja": "太平洋標準時",
		"de": "Pazifische Normalzeit",
		"pt": "Horário Padrão do Pacífico",
		"zh": "太平洋标准时间",
	},
	"America/Chicago": {
		"en": "Central Standard Time",
		"ja": "中部標準時",
		"de": "Zentrale Normalzeit",
		"pt": "Horário Padrão Central",
		"zh": "中部标准时间",
	},
	"America/Denver": {
		"en": "Mountain Standard Time",
		"ja": "山岳部標準時",
		"de": "Mountain Normalzeit",
		"pt": "Horário Padrão das Montanhas",
		"zh": "山地标准时间",
	},
	"America/Sao_Paulo": {
		"en": "Brasilia Standard Time",
		"ja": "ブラジリア時間",
		"de": "Brasilianische Normalzeit",
		"pt": "Horário Padrão de Brasília",
		"zh": "巴西利亚标准时间",
	},
	"Europe/London": {
		"en": "Greenwich Mean Time",
		"ja": "グリニッジ標準時",
		"de": "Greenwich-Normalzeit",
		"pt": "Horário de Greenwich",
		"zh": "格林尼治标准时间",
	},
	"Europe/Berlin": {
		"en": "Central European Standard Time",
		"ja": "中央ヨーロッパ標準時",
		"de": "Mitteleuropäische Zeit",
		"pt": "Horário Padrão da Europa Central",
		"zh": "中欧标准时间",
	},
	"Europe/Paris": {
		"en": "Central European Standard Time",
		"ja": "中央ヨーロッパ標準時",
		"de": "Mitteleuropäische Zeit",
		"pt": "Horário Padrão da Europa Central",
		"zh": "中欧标准时间",
	},
	"Europe/Madrid": {
		"en": "Central European Standard Time",
		"ja": "中央ヨーロッパ標準時",
		"de": "Mitteleuropäische Zeit",
		"pt": "Horário Padrão da Europa Central",
		"zh": "中欧标准时间",
	},
	"Europe/Rome": {
		"en": "Central European Standard Time",
		"ja": "中央ヨーロッパ標準時",
		"de": "Mitteleuropäische Zeit",
		"pt": "Horário Padrão da Europa Central",
		"zh": "中欧标准时间",
	},
	"Asia/Tokyo": {
		"en": "Japan Standard Time",
		"ja": "日本標準時",
		"de": "Japanische Normalzeit",
		"pt": "Horário Padrão do Japão",
		"zh": "日本标准时间",
	},
	"Asia/Seoul": {
		"en": "Korean Standard Time",
		"ja": "韓国標準時",
		"de": "Koreanische Normalzeit",
		"pt": "Horário Padrão da Coreia",
		"zh": "韩国标准时间",
	},
	"Asia/Shanghai": {
		"en": "China Standard Time",
		"ja": "中国標準時",
		"de": "Chinesische Normalzeit",
		"pt": "Horário Padrão da China",
		"zh": "中国标准时间",
	},
	"Asia/Singapore": {
		"en": "Singapore Standard Time",
		"ja": "シンガポール標準時",
		"de": "Singapur-Normalzeit",
		"pt": "Horário Padrão de Singapura",
		"zh": "新加坡标准时间",
	},
	"Asia/Bangkok": {
		"en": "Indochina Time",
		"ja": "インドシナ時間",
		"de": "Indochina-Zeit",
		"pt": "Horário da Indochina",
		"zh": "印度支那时间",
	},
	"Asia/Jakarta": {
		"en": "Western Indonesian Time",
		"ja": "西インドネシア時間",
		"de": "Westindonesische Zeit",
		"pt": "Horário do Oeste da Indonésia",
		"zh": "印度尼西亚西部时间",
	},
	"Asia/Dubai": {
		"en": "Gulf Standard Time",
		"ja": "湾岸標準時",
		"de": "Golf-Normalzeit",
		"pt": "Horário Padrão do Golfo",
		"zh": "海湾标准时间",
	},
	"Australia/Sydney": {
		"en": "Australian Eastern Standard Time",
		"ja": "オーストラリア東部標準時",
		"de": "Ostaustralische Normalzeit",
		"pt": "Horário Padrão do Leste da Austrália",
		"zh": "澳大利亚东部标准时间",
	},
}

// tzLabel 取画像时区在 Date#toString 里的显示名。
//
// 回退链：画像语种本地化名 → 英文通用名 → 时区 ID 城市段（Asia/Tokyo → Tokyo）。
// 全为空时返回空串，此时 JS 侧只输出 "GMT+0900" 而不带括号段。
func tzLabel(timezone, locale string) string {
	if timezone == "" {
		return ""
	}
	if names, ok := tzDisplayNames[timezone]; ok {
		if lang := baseLang(locale); lang != "" {
			if v, ok := names[lang]; ok && v != "" {
				return v
			}
		}
		if v, ok := names["en"]; ok && v != "" {
			return v
		}
	}
	if i := strings.LastIndex(timezone, "/"); i >= 0 && i < len(timezone)-1 {
		return strings.ReplaceAll(timezone[i+1:], "_", " ")
	}
	return timezone
}

// baseLang "ja-JP" → "ja"；空串与非 "-xx" 形式原样小写返回。
func baseLang(locale string) string {
	locale = strings.TrimSpace(locale)
	if i := strings.Index(locale, "-"); i > 0 {
		return strings.ToLower(locale[:i])
	}
	return strings.ToLower(locale)
}
