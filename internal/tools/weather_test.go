package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func weatherTestServers(t *testing.T) (geo, fc *httptest.Server) {
	t.Helper()
	geo = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "name=") {
			t.Errorf("地理编码缺少 name 参数: %s", r.URL.RawQuery)
		}
		// 回显请求的城市名，验证参数传递；经 json.Marshal 构造响应，避免手工拼接字节。
		resp := map[string]any{"results": []map[string]any{{
			"name":      r.URL.Query().Get("name") + "市",
			"admin1":    "",
			"country":   "中国",
			"latitude":  31.22,
			"longitude": 121.46,
		}}}
		b, err := json.Marshal(resp)
		if err != nil {
			t.Errorf("构造地理编码响应失败: %v", err)
			return
		}
		w.Write(b)
	}))
	fc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"current":{"temperature_2m":26.4,"apparent_temperature":28.1,
			"relative_humidity_2m":70,"weather_code":3,"wind_speed_10m":12.2},
			"daily":{"time":["2099-01-01","2099-01-02","2099-01-03"],
			"weather_code":[61,0,95],"temperature_2m_max":[28,30,29],"temperature_2m_min":[22,23,24]}}`))
	}))
	t.Cleanup(func() { geo.Close(); fc.Close() })
	return geo, fc
}

func TestWeatherToolHappyPath(t *testing.T) {
	geo, fc := weatherTestServers(t)
	wt := &WeatherTool{DefaultCity: "上海", GeocodeBase: geo.URL, ForecastBase: fc.URL}
	res, err := wt.Call(context.Background(), nil) // 不传 city → 用默认城市
	if err != nil {
		t.Fatalf("get_weather: %v", err)
	}
	for _, want := range []string{"上海市 中国", "阴", "26°C", "今天：小雨", "明天：晴", "后天：雷阵雨"} {
		if !strings.Contains(res, want) {
			t.Errorf("输出缺少 %q：%s", want, res)
		}
	}
}

func TestWeatherToolExplicitCity(t *testing.T) {
	geo, fc := weatherTestServers(t)
	wt := &WeatherTool{GeocodeBase: geo.URL, ForecastBase: fc.URL}
	res, err := wt.Call(context.Background(), map[string]any{"city": " 北京 "})
	if err != nil || !strings.Contains(res, "北京市") {
		t.Fatalf("显式城市查询: %v %q", err, res)
	}
}

func TestWeatherToolCityAndErrors(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`)) // 无结果
	}))
	defer geo.Close()
	wt := &WeatherTool{GeocodeBase: geo.URL, ForecastBase: "http://unused"}

	// 未指明城市且无默认 → 提示询问。
	if _, err := wt.Call(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "询问用户") {
		t.Fatalf("无城市应提示询问: %v", err)
	}
	// 城市不存在 → 明确报错。
	if _, err := wt.Call(context.Background(), map[string]any{"city": "亚特兰蒂斯"}); err == nil || !strings.Contains(err.Error(), "未找到城市") {
		t.Fatalf("未知城市应报错: %v", err)
	}
}

func TestWMODesc(t *testing.T) {
	if wmoDesc(0) != "晴" || wmoDesc(61) != "小雨" {
		t.Fatal("常见天气代码翻译错误")
	}
	if !strings.Contains(wmoDesc(42), "未知") {
		t.Fatal("未定义代码应返回未知")
	}
}
