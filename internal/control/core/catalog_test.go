package core_test

import "testing"

// 目录带品牌名（docs/05 §6 第 6 步：前台品牌名来自后端）；未配置时取默认值，不为空。
func TestCatalogCarriesBrand(t *testing.T) {
	x := newH(t)
	var c struct {
		Brand string `json:"brand"`
		Plans []struct {
			ID string `json:"id"`
		} `json:"plans"`
	}
	x.ok(x.do(who{}, "GET", "/api/catalog", nil), 200, &c)
	if c.Brand != "ISP 代理" || len(c.Plans) != 1 {
		t.Fatalf("目录应带默认品牌名与上架套餐: %+v", c)
	}
}
