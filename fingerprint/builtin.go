package fingerprint

// BuiltinProfiles 内置设备画像模板（真实设备规格）。
//
// 数据来源与更新机制（ADR-0009）：
//  1. 首批为人工整理的主流真实机型规格（Windows 11 / macOS 14 / Ubuntu 22.04）；
//  2. MarketShare 为该机型在目标国家的近似占比，用于加权采样使分布贴近真实市场；
//  3. TODO(P1)：接入采集器（客户端匿名上报 + 公开硬件统计）自动扩充，
//     每国家模板池规模下限由 LibraryConfig.MinPoolPerCountry 约束（默认 50）。
func BuiltinProfiles() []Profile {
	return []Profile{
		{
			ID: "us-win-dell-xps-8930", Vendor: "Dell", Model: "XPS 8930",
			OS:       OSInfo{Name: "Windows", Version: "11", Build: "10.0.22631", Platform: "Win32", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 8, Architecture: "x86_64", Vendor: "Intel", DeviceMemory: 8, MemApproxGB: 16},
			MemoryGB: 16,
			Screen:   ScreenInfo{Width: 1920, Height: 1080, AvailWidth: 1920, AvailHeight: 1040, DPR: 1, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Intel)", Renderer: "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)",
				WebGLVendor: "Intel Inc.", WebGLRenderer: "Intel(R) UHD Graphics 630",
				DriverVersions: []string{"30.0.101.1994", "31.0.101.2114", "31.0.101.2130"},
			},
			Timezone: "America/New_York", UTCOffset: -5, Locale: "en-US", Languages: []string{"en-US", "en"},
			Country: "US", City: "New York",
			Fonts:   windowsFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Windows"`, PlatformVersion: `"15.0.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 12.5,
		},
		{
			ID: "us-win-hp-pavilion-15", Vendor: "HP", Model: "Pavilion 15-eg2xxx",
			OS:       OSInfo{Name: "Windows", Version: "11", Build: "10.0.22631", Platform: "Win32", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 12, Architecture: "x86_64", Vendor: "Intel", DeviceMemory: 8, MemApproxGB: 16},
			MemoryGB: 16,
			Screen:   ScreenInfo{Width: 1536, Height: 864, AvailWidth: 1536, AvailHeight: 824, DPR: 1.25, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Intel)", Renderer: "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)",
				WebGLVendor: "Intel Inc.", WebGLRenderer: "Intel(R) Iris(R) Xe Graphics",
				DriverVersions: []string{"31.0.101.4575", "31.0.101.4676"},
			},
			Timezone: "America/Los_Angeles", UTCOffset: -8, Locale: "en-US", Languages: []string{"en-US", "en"},
			Country: "US", City: "Los Angeles",
			Fonts:   windowsFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Windows"`, PlatformVersion: `"15.0.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 9.8,
		},
		{
			ID: "us-mac-mbp-m2", Vendor: "Apple", Model: "MacBook Pro 14 (M2 Pro)",
			OS:       OSInfo{Name: "macOS", Version: "14.4", Build: "23E224", Platform: "MacIntel", Arch: "arm64"},
			CPU:      CPUInfo{Cores: 10, Architecture: "arm64", Vendor: "Apple", DeviceMemory: 8, MemApproxGB: 16},
			MemoryGB: 16,
			Screen:   ScreenInfo{Width: 1728, Height: 1117, AvailWidth: 1728, AvailHeight: 1077, DPR: 2, ColorDepth: 30, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Apple)", Renderer: "ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Pro, Unspecified Version)",
				WebGLVendor: "Apple Inc.", WebGLRenderer: "Apple M2 Pro",
				DriverVersions: []string{"Metal 3.1", "Metal 3.2"},
			},
			Timezone: "America/New_York", UTCOffset: -5, Locale: "en-US", Languages: []string{"en-US", "en"},
			Country: "US", City: "New York",
			Fonts:   macOSFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"macOS"`, PlatformVersion: `"14.4.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 8.1,
		},
		{
			ID: "uk-win-lenovo-thinkpad-t14", Vendor: "Lenovo", Model: "ThinkPad T14 Gen 3",
			OS:       OSInfo{Name: "Windows", Version: "11", Build: "10.0.22631", Platform: "Win32", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 8, Architecture: "x86_64", Vendor: "AMD", DeviceMemory: 8, MemApproxGB: 16},
			MemoryGB: 16,
			Screen:   ScreenInfo{Width: 1920, Height: 1200, AvailWidth: 1920, AvailHeight: 1160, DPR: 1, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (AMD)", Renderer: "ANGLE (AMD, AMD Radeon(TM) Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)",
				WebGLVendor: "AMD", WebGLRenderer: "AMD Radeon(TM) Graphics",
				DriverVersions: []string{"31.0.22023.1004", "32.0.11015.1009"},
			},
			Timezone: "Europe/London", UTCOffset: 0, Locale: "en-GB", Languages: []string{"en-GB", "en"},
			Country: "GB", City: "London",
			Fonts:   windowsFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Windows"`, PlatformVersion: `"15.0.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 6.4,
		},
		{
			ID: "de-win-acer-aspire-5", Vendor: "Acer", Model: "Aspire 5 A515-57",
			OS:       OSInfo{Name: "Windows", Version: "11", Build: "10.0.22621", Platform: "Win32", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 8, Architecture: "x86_64", Vendor: "Intel", DeviceMemory: 8, MemApproxGB: 8},
			MemoryGB: 8,
			Screen:   ScreenInfo{Width: 1920, Height: 1080, AvailWidth: 1920, AvailHeight: 1040, DPR: 1, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Intel)", Renderer: "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)",
				WebGLVendor: "Intel Inc.", WebGLRenderer: "Intel(R) Iris(R) Xe Graphics",
				DriverVersions: []string{"31.0.101.4577", "31.0.101.4676"},
			},
			Timezone: "Europe/Berlin", UTCOffset: 1, Locale: "de-DE", Languages: []string{"de-DE", "de", "en-US"},
			Country: "DE", City: "Berlin",
			Fonts:   windowsFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Windows"`, PlatformVersion: `"15.0.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 5.2,
		},
		{
			ID: "jp-mac-mba-m1", Vendor: "Apple", Model: "MacBook Air (M1)",
			OS:       OSInfo{Name: "macOS", Version: "14.3", Build: "23D56", Platform: "MacIntel", Arch: "arm64"},
			CPU:      CPUInfo{Cores: 8, Architecture: "arm64", Vendor: "Apple", DeviceMemory: 8, MemApproxGB: 8},
			MemoryGB: 8,
			Screen:   ScreenInfo{Width: 1440, Height: 900, AvailWidth: 1440, AvailHeight: 870, DPR: 2, ColorDepth: 30, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Apple)", Renderer: "ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)",
				WebGLVendor: "Apple Inc.", WebGLRenderer: "Apple M1",
				DriverVersions: []string{"Metal 3.1", "Metal 3.2"},
			},
			Timezone: "Asia/Tokyo", UTCOffset: 9, Locale: "ja-JP", Languages: []string{"ja-JP", "ja", "en-US"},
			Country: "JP", City: "Tokyo",
			Fonts:   macOSFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"macOS"`, PlatformVersion: `"14.3.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 4.7,
		},
		{
			ID: "br-win-positivo-motion", Vendor: "Positivo", Model: "Motion Q4640",
			OS:       OSInfo{Name: "Windows", Version: "11", Build: "10.0.22631", Platform: "Win32", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 4, Architecture: "x86_64", Vendor: "Intel", DeviceMemory: 4, MemApproxGB: 8},
			MemoryGB: 8,
			Screen:   ScreenInfo{Width: 1366, Height: 768, AvailWidth: 1366, AvailHeight: 728, DPR: 1, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Intel)", Renderer: "ANGLE (Intel, Intel(R) UHD Graphics 620 Direct3D11 vs_5_0 ps_5_0, D3D11)",
				WebGLVendor: "Intel Inc.", WebGLRenderer: "Intel(R) UHD Graphics 620",
				DriverVersions: []string{"30.0.101.1994", "31.0.101.2114"},
			},
			Timezone: "America/Sao_Paulo", UTCOffset: -3, Locale: "pt-BR", Languages: []string{"pt-BR", "pt", "en-US"},
			Country: "BR", City: "Sao Paulo",
			Fonts:   windowsFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Windows"`, PlatformVersion: `"15.0.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 3.9,
		},
		{
			ID: "us-linux-ubuntu-thinkcentre", Vendor: "Lenovo", Model: "ThinkCentre M70q",
			OS:       OSInfo{Name: "Linux", Version: "22.04", Build: "", Platform: "Linux x86_64", Arch: "x86_64"},
			CPU:      CPUInfo{Cores: 6, Architecture: "x86_64", Vendor: "Intel", DeviceMemory: 8, MemApproxGB: 16},
			MemoryGB: 16,
			Screen:   ScreenInfo{Width: 1920, Height: 1080, AvailWidth: 1920, AvailHeight: 1040, DPR: 1, ColorDepth: 24, TouchPoints: 0},
			GPU: GPUInfo{
				Vendor: "Google Inc. (Intel)", Renderer: "ANGLE (Intel, Mesa Intel(R) UHD Graphics 630 (CFL GT2), OpenGL 4.6)",
				WebGLVendor: "Intel", WebGLRenderer: "Mesa Intel(R) UHD Graphics 630 (CFL GT2)",
				DriverVersions: []string{"4.6 (Core Profile) Mesa 23.2.1", "4.6 (Core Profile) Mesa 24.0.5"},
			},
			Timezone: "America/Chicago", UTCOffset: -6, Locale: "en-US", Languages: []string{"en-US", "en"},
			Country: "US", City: "Chicago",
			Fonts:   linuxFonts(),
			Plugins: defaultPlugins(),
			UA:      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UACH: UACHInfo{
				Brands:   []string{`"Chromium";v="131"`, `"Not_A Brand";v="24"`, `"Google Chrome";v="131"`},
				Platform: `"Linux"`, PlatformVersion: `"6.5.0"`, Mobile: false, Model: `""`,
			},
			WebRTC:      WebRTCInfo{Mode: "replace_with_proxy"},
			MarketShare: 2.1,
		},
	}
}

func windowsFonts() []string {
	return []string{
		"Arial", "Arial Black", "Calibri", "Segoe UI", "Times New Roman", "Verdana",
		"Consolas", "Georgia", "Trebuchet MS", "Tahoma", "Microsoft YaHei", "SimSun",
	}
}

func macOSFonts() []string {
	return []string{
		"Helvetica Neue", "SF Pro Text", "SF Pro Display", "Menlo", "Times", "Arial",
		"Geneva", "Courier New", "PingFang SC", "Hiragino Sans", "Avenir Next", "Monaco",
	}
}

func linuxFonts() []string {
	return []string{
		"DejaVu Sans", "DejaVu Serif", "Ubuntu", "Liberation Sans", "Liberation Serif",
		"Noto Sans CJK SC", "FreeMono", "FreeSans", "Cantarell", "Purisa",
	}
}

// defaultPlugins 统一插件集：所有环境共用同一套，避免「混用导致同画像不一致」（反例 5）。
func defaultPlugins() []Plugin {
	return []Plugin{
		{Name: "PDF Viewer", Filename: "internal-pdf-viewer", Description: "Portable Document Format", MimeTypes: []string{"application/pdf", "text/pdf"}},
		{Name: "Chrome PDF Viewer", Filename: "internal-pdf-viewer", Description: "Portable Document Format", MimeTypes: []string{"application/pdf", "text/pdf"}},
		{Name: "Chromium PDF Viewer", Filename: "internal-pdf-viewer", Description: "Portable Document Format", MimeTypes: []string{"application/pdf", "text/pdf"}},
		{Name: "Microsoft Edge PDF Viewer", Filename: "internal-pdf-viewer", Description: "Portable Document Format", MimeTypes: []string{"application/pdf", "text/pdf"}},
		{Name: "WebKit built-in PDF", Filename: "internal-pdf-viewer", Description: "Portable Document Format", MimeTypes: []string{"application/pdf", "text/pdf"}},
	}
}
