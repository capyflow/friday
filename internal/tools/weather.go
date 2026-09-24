package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WeatherTool 查询城市天气（Open-Meteo 免密钥接口，全球城市中文地理编码）。
type WeatherTool struct {
	// DefaultCity 用户未指明城市时使用的默认城市（config weather.defaultCity）；
	// 为空且用户未指明时返回提示，由 agent 询问用户所在城市。
	DefaultCity string
	// GeocodeBase/ForecastBase 接口基地址（测试可替换为 httptest 服务）。
	GeocodeBase  string
	ForecastBase string
	// HTTP 请求客户端（nil 时用 10 秒超时默认）。
	HTTP *http.Client
}

func (t *WeatherTool) Name() string { return "get_weather" }

func (t *WeatherTool) Description() string {
	return "查询某个城市当前天气与未来三天预报（天气现象、气温、湿度、风力）。" +
		"用户问天气、要不要带伞、穿什么时使用。city 不填时查默认城市（未配置默认城市则需先问用户）。"
}

func (t *WeatherTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"type": "string", "description": "城市名，如：上海 / 北京 / 东京；留空用默认城市"},
		},
	}
}

func (t *WeatherTool) Call(ctx context.Context, args map[string]any) (string, error) {
	city, _ := args["city"].(string)
	city = strings.TrimSpace(city)
	if city == "" {
		city = t.DefaultCity
	}
	if city == "" {
		return "", fmt.Errorf("未指明城市且未配置默认城市（weather.defaultCity），请先询问用户所在城市")
	}

	geo, err := t.geocode(ctx, city)
	if err != nil {
		return "", err
	}

	var fc forecastResp
	if err := t.getJSON(ctx, t.forecastURL(geo.Latitude, geo.Longitude), &fc); err != nil {
		return "", fmt.Errorf("查询天气失败: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s 当前天气：%s %.0f°C（体感 %.0f°C，湿度 %.0f%%，风速 %.0f km/h）",
		geo.Label(), wmoDesc(fc.Current.WeatherCode), fc.Current.Temperature,
		fc.Current.Apparent, fc.Current.Humidity, fc.Current.WindSpeed)
	if n := len(fc.Daily.Time); n > 0 {
		today := time.Now()
		b.WriteString("\n未来预报：")
		for i := 0; i < n && i < 3; i++ {
			day := fmtDay(today, fc.Daily.Time[i], i)
			fmt.Fprintf(&b, "\n- %s：%s %.0f~%.0f°C", day,
				wmoDesc(fc.Daily.WeatherCode[i]),
				fc.Daily.TempMin[i], fc.Daily.TempMax[i])
		}
	}
	return b.String(), nil
}

// geoPlace 是地理编码返回的城市（取第一个结果）。
type geoPlace struct {
	Name      string  `json:"name"`
	Admin1    string  `json:"admin1"`
	Country   string  `json:"country"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Label 返回带省/国家的城市全名，如 "上海市 上海市 中国"。
func (p geoPlace) Label() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{p.Name, p.Admin1, p.Country} {
		s = strings.TrimSpace(s)
		if s != "" && (len(parts) == 0 || parts[len(parts)-1] != s) {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

func (t *WeatherTool) geocode(ctx context.Context, city string) (geoPlace, error) {
	var res struct {
		Results []geoPlace `json:"results"`
	}
	u := strings.TrimRight(t.geocodeBase(), "/") + "/search?" +
		url.Values{"name": {city}, "count": {"1"}, "language": {"zh"}, "format": {"json"}}.Encode()
	if err := t.getJSON(ctx, u, &res); err != nil {
		return geoPlace{}, fmt.Errorf("城市地理编码失败: %w", err)
	}
	if len(res.Results) == 0 {
		return geoPlace{}, fmt.Errorf("未找到城市 %q，请确认城市名", city)
	}
	return res.Results[0], nil
}

func (t *WeatherTool) forecastURL(lat, lon float64) string {
	return strings.TrimRight(t.forecastBase(), "/") + "/forecast?" + url.Values{
		"latitude":      {strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":     {strconv.FormatFloat(lon, 'f', 4, 64)},
		"current":       {"temperature_2m,apparent_temperature,relative_humidity_2m,weather_code,wind_speed_10m"},
		"daily":         {"weather_code,temperature_2m_max,temperature_2m_min"},
		"forecast_days": {"3"},
		"timezone":      {"auto"},
	}.Encode()
}

type forecastResp struct {
	Current struct {
		Temperature float64 `json:"temperature_2m"`
		Apparent    float64 `json:"apparent_temperature"`
		Humidity    float64 `json:"relative_humidity_2m"`
		WeatherCode int     `json:"weather_code"`
		WindSpeed   float64 `json:"wind_speed_10m"`
	} `json:"current"`
	Daily struct {
		Time        []string  `json:"time"`
		WeatherCode []int     `json:"weather_code"`
		TempMax     []float64 `json:"temperature_2m_max"`
		TempMin     []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

// getJSON 请求 u 并把响应解析为 v。
func (t *WeatherTool) getJSON(ctx context.Context, u string, v any) error {
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("接口返回 %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (t *WeatherTool) geocodeBase() string {
	if t.GeocodeBase != "" {
		return t.GeocodeBase
	}
	return "https://geocoding-api.open-meteo.com/v1"
}

func (t *WeatherTool) forecastBase() string {
	if t.ForecastBase != "" {
		return t.ForecastBase
	}
	return "https://api.open-meteo.com/v1"
}

// fmtDay 把预报日期转成"今天/明天/后天/具体日期"。
func fmtDay(now time.Time, date string, idx int) string {
	switch idx {
	case 0:
		return "今天"
	case 1:
		return "明天"
	case 2:
		return "后天"
	}
	d, err := time.ParseInLocation("2006-01-02", date, now.Location())
	if err != nil {
		return date
	}
	return d.Format("01-02")
}

// wmoDesc 把 WMO 天气代码翻译成中文描述。
func wmoDesc(code int) string {
	switch code {
	case 0:
		return "晴"
	case 1:
		return "大部晴朗"
	case 2:
		return "局部多云"
	case 3:
		return "阴"
	case 45, 48:
		return "雾"
	case 51:
		return "毛毛雨（轻）"
	case 53:
		return "毛毛雨"
	case 55:
		return "毛毛雨（浓）"
	case 56, 57:
		return "冻毛毛雨"
	case 61:
		return "小雨"
	case 63:
		return "中雨"
	case 65:
		return "大雨"
	case 66, 67:
		return "冻雨"
	case 71:
		return "小雪"
	case 73:
		return "中雪"
	case 75:
		return "大雪"
	case 77:
		return "雪粒"
	case 80:
		return "阵雨"
	case 81:
		return "阵雨（较强）"
	case 82:
		return "强阵雨"
	case 85, 86:
		return "阵雪"
	case 95:
		return "雷阵雨"
	case 96, 99:
		return "雷阵雨伴冰雹"
	default:
		return "未知（代码 " + strconv.Itoa(code) + "）"
	}
}
