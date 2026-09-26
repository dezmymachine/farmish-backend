package geo

// RegionWithDistricts pairs a region with its district suggestions.
type RegionWithDistricts struct {
	Name      string
	Districts []string
}

// RegionDistricts holds district suggestions per region, sorted by region
// name. Ported from the legacy app's ghana-locations list: districts may
// appear under two regions (kept in both), but never twice in one. These
// are suggestions only: unknown districts are still accepted (DOMAIN §10).
var RegionDistricts = []RegionWithDistricts{
	{Name: "Ahafo", Districts: []string{"Goaso", "Asunafo North", "Asunafo South", "Asutifi North", "Asutifi South", "Tano North", "Tano South"}},
	{Name: "Ashanti", Districts: []string{"Kumasi Metropolitan", "Obuasi Municipal", "Mampong Municipal", "Ejisu-Juaben", "Kwabre East", "Asokwa", "Atwima Kwanwoma", "Atwima Nwabiagya", "Bekwai Municipal", "Bosomtwe", "Offinso North", "Offinso South", "Sekyere South", "Sekyere East", "Ahafo Ano North", "Ahafo Ano South"}},
	{Name: "Bono", Districts: []string{"Sunyani Municipal", "Sunyani West", "Berekum Municipal", "Berekum East", "Dormaa Central", "Dormaa East", "Dormaa West", "Jaman North", "Jaman South", "Banda", "Tain", "Wenchi"}},
	{Name: "Bono East", Districts: []string{"Techiman Municipal", "Techiman North", "Nkoranza South", "Nkoranza North", "Kintampo North", "Kintampo South", "Atebubu-Amantin", "Pru East", "Pru West", "Sene East", "Sene West"}},
	{Name: "Central", Districts: []string{"Cape Coast Metropolitan", "Mfantseman", "Gomoa West", "Gomoa East", "Awutu Senya", "Awutu Senya East", "Agona West", "Agona East", "Assin North", "Assin South", "Abura-Asebu-Kwamankese", "Asikuma-Odoben-Brakwa", "Ajumako-Enyan-Essiam", "Twifo-Ati Morkwa", "Upper Denkyira West", "Upper Denkyira East", "Komenda-Edina-Eguafo-Abirem"}},
	{Name: "Eastern", Districts: []string{"Koforidua", "New Juaben South", "New Juaben North", "Akuapim South", "Akuapim North", "Suhum", "Nsawam-Adoagyiri", "Upper West Akim", "Lower West Akim", "Kwahu West", "Kwahu South", "Kwahu East", "Fanteakwa", "Yilo Krobo", "Manya Krobo", "Asuogyaman", "Afram Plains"}},
	{Name: "Greater Accra", Districts: []string{"Accra Metropolitan", "Tema Metropolitan", "Ga East", "Ga West", "Ga South", "Adenta", "Ashaiman", "Ledzokuku-Krowor", "Kpone-Katamanso", "Ningo-Prampram", "Shai-Osudoku"}},
	{Name: "North East", Districts: []string{"Nalerigu", "Bunkpurugu-Nyankpanduri", "Chereponi", "Yunyoo-Nasuan", "Gushegu", "Karaga"}},
	{Name: "Northern", Districts: []string{"Tamale Metropolitan", "Sagnarigu", "Savelugu-Nanton", "Nanton", "Karaga", "Gushegu", "Saboba", "Chereponi", "Bimbilla", "Nanumba North", "Nanumba South", "Yendi Municipal", "Mion", "Tolon", "Kumbungu"}},
	{Name: "Oti", Districts: []string{"Dambai", "Krachi East", "Krachi West", "Krachi Nchumuru", "Nkwanta North", "Nkwanta South", "Biakoye", "Jasikan", "Kadjebi"}},
	{Name: "Savannah", Districts: []string{"Damongo", "Bole", "Sawla-Tuna-Kalba", "West Gonja", "North Gonja", "Central Gonja", "East Gonja"}},
	{Name: "Upper East", Districts: []string{"Bolgatanga Municipal", "Bolgatanga East", "Bongo", "Talensi", "Nabdam", "Bawku West", "Bawku Municipal", "Binduri", "Garu", "Tempane", "Pusiga", "Builsa South", "Builsa North"}},
	{Name: "Upper West", Districts: []string{"Wa Municipal", "Wa East", "Wa West", "Nadowli-Kaleo", "Jirapa", "Lambussie", "Sissala East", "Sissala West", "Lawra", "Nandom", "Daffiama-Bussie-Issa"}},
	{Name: "Volta", Districts: []string{"Ho Municipal", "Ho West", "Ho Central", "Keta", "Ketu South", "Ketu North", "Akatsi North", "Akatsi South", "Agotime-Ziope", "Adaklu", "Central Tongu", "North Tongu", "South Tongu", "Biakoye", "Jasikan", "Kadjebi", "Krachi East", "Krachi West", "Krachi Nchumuru", "Nkwanta North", "Nkwanta South"}},
	{Name: "Western", Districts: []string{"Sekondi-Takoradi Metropolitan", "Shama", "Nzema East", "Ellembele", "Jomoro", "Ahanta West", "Wassa East", "Wassa Amenfi East", "Wassa Amenfi West", "Prestea-Huni Valley", "Mpohor", "Tarkwa-Nsuaem"}},
	{Name: "Western North", Districts: []string{"Sefwi Wiawso", "Sefwi Akontombra", "Sefwi-Bibiani", "Juaboso", "Bia East", "Bia West", "Suaman", "Bodi"}},
}
