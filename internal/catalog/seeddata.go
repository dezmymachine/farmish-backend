package catalog

// Seed data ported from the legacy app (DOMAIN §9): 12 parents in order
// with their children in order, icons, groups and typed attributes.
// Attribute keys are snake_case of the label without units. Sort orders are
// 1-based throughout. Go literals, so the compiler checks them.
type SeedAttribute struct {
	Key      string
	Label    string
	Type     string
	Options  []string
	Required bool
}

type SeedCategory struct {
	Name       string
	Slug       string
	Icon       string
	Group      string
	Children   []string
	Attributes []SeedAttribute
}

// SeedCategories is the exact DOMAIN §9 tree.
var SeedCategories = []SeedCategory{
	{
		Name: "Seeds & Seedlings", Slug: "seeds-seedlings", Icon: "🌱", Group: "quality",
		Children: []string{"Cereal Seeds", "Vegetable Seeds", "Fruit Seeds", "Tree Crop Seedlings", "Flowers & Ornamentals", "Pasture Seeds", "Seedlings & Cuttings"},
		Attributes: []SeedAttribute{
			{Key: "germination_rate", Label: "Germination Rate", Type: "select", Options: []string{"95%+", "90-95%", "85-90%", "Below 85%"}},
			{Key: "treatment", Label: "Treatment", Type: "select", Options: []string{"Treated", "Untreated", "Organic"}},
			{Key: "certified", Label: "Certified", Type: "boolean"},
		},
	},
	{
		Name: "Fertilizers & Chemicals", Slug: "fertilizers-chemicals", Icon: "🧪", Group: "quality",
		Children: []string{"NPK Fertilizers", "Organic Fertilizers", "Liquid Fertilizers", "Pesticides", "Herbicides", "Fungicides", "Growth Regulators"},
		Attributes: []SeedAttribute{
			{Key: "npk_ratio", Label: "NPK Ratio", Type: "text"},
			{Key: "organic", Label: "Organic", Type: "boolean"},
			{Key: "application_method", Label: "Application Method", Type: "select", Options: []string{"Foliar", "Soil", "Fertigation", "Broadcast"}},
		},
	},
	{
		Name: "Farm Machinery", Slug: "farm-machinery", Icon: "🚜", Group: "equipment",
		Children: []string{"Tractors", "Tillers & Cultivators", "Harvesters", "Sprayers", "Irrigation Equipment", "Processing Machines", "Tools & Implements"},
		Attributes: []SeedAttribute{
			{Key: "horsepower", Label: "Horsepower", Type: "text"},
			{Key: "fuel_type", Label: "Fuel Type", Type: "select", Options: []string{"Diesel", "Petrol", "Electric", "Hybrid"}},
			{Key: "year_of_manufacture", Label: "Year of Manufacture", Type: "number"},
		},
	},
	{
		Name: "Livestock & Poultry", Slug: "livestock-poultry", Icon: "🐄", Group: "livestock",
		Children: []string{"Cattle", "Sheep & Goats", "Pigs", "Chickens", "Turkeys", "Rabbits", "Grasscutters", "Exotic Animals"},
		Attributes: []SeedAttribute{
			{Key: "breed", Label: "Breed", Type: "text"},
			{Key: "vaccinated", Label: "Vaccinated", Type: "boolean"},
			{Key: "weight_kg", Label: "Weight (kg)", Type: "number"},
		},
	},
	{
		Name: "Feeds & Supplements", Slug: "feeds-supplements", Icon: "🌾", Group: "quality",
		Children: []string{"Poultry Feed", "Livestock Feed", "Fish Feed", "Pet Food", "Feed Supplements", "Vitamins & Minerals", "Feed Ingredients"},
		Attributes: []SeedAttribute{
			{Key: "protein_content", Label: "Protein Content", Type: "text"},
			{Key: "feed_type", Label: "Feed Type", Type: "select", Options: []string{"Starter", "Grower", "Finisher", "Layer", "Breeder"}},
		},
	},
	{
		Name: "Fresh Produce", Slug: "fresh-produce", Icon: "🥬", Group: "quality",
		Children: []string{"Vegetables", "Fruits", "Root Crops", "Grains & Cereals", "Legumes", "Herbs & Spices", "Honey & Bee Products"},
		Attributes: []SeedAttribute{
			{Key: "harvest_date", Label: "Harvest Date", Type: "date"},
			{Key: "organic", Label: "Organic", Type: "boolean"},
			{Key: "shelf_life_days", Label: "Shelf Life (days)", Type: "number"},
		},
	},
	{
		Name: "Processed Foods", Slug: "processed-foods", Icon: "🥫", Group: "quality",
		Children: []string{"Dried & Smoked Fish", "Cooking Oils", "Flours & Powders", "Packaged Foods", "Beverages", "Snacks", "Preserves & Sauces"},
		Attributes: []SeedAttribute{
			{Key: "expiry_date", Label: "Expiry Date", Type: "date"},
			{Key: "storage_type", Label: "Storage Type", Type: "select", Options: []string{"Room Temperature", "Refrigerated", "Frozen"}},
			{Key: "packaging", Label: "Packaging", Type: "text"},
		},
	},
	{
		Name: "Farm Labour", Slug: "farm-labour", Icon: "👷", Group: "service",
		Children: []string{"Field Workers", "Harvesting Services", "Planting Services", "Spraying Services", "Consulting", "Agribusiness Training"},
		Attributes: []SeedAttribute{
			{Key: "experience_years", Label: "Experience (years)", Type: "number"},
			{Key: "certified", Label: "Certified", Type: "boolean"},
			{Key: "availability", Label: "Availability", Type: "select", Options: []string{"Full-time", "Part-time", "Contract", "Seasonal"}},
		},
	},
	{
		Name: "Land & Leasing", Slug: "land-leasing", Icon: "🌳", Group: "land",
		Children: []string{"Farmland for Rent", "Farmland for Sale", "Warehouse Space", "Cold Storage", "Office Space", "Greenhouses"},
		Attributes: []SeedAttribute{
			{Key: "land_size_acres", Label: "Land Size (acres)", Type: "number", Required: true},
			{Key: "soil_type", Label: "Soil Type", Type: "select", Options: []string{"Loamy", "Sandy", "Clay", "Silt", "Mixed"}},
			{Key: "water_source", Label: "Water Source", Type: "select", Options: []string{"River", "Borehole", "Rain-fed", "Irrigation", "None"}},
		},
	},
	{
		Name: "Storage & Packaging", Slug: "storage-packaging", Icon: "📦", Group: "equipment",
		Children: []string{"Sacks & Bags", "Crates & Baskets", "Packaging Materials", "Storage Containers", "Labels & Stickers"},
		Attributes: []SeedAttribute{
			{Key: "material", Label: "Material", Type: "select", Options: []string{"Plastic", "Jute", "Polypropylene", "Paper", "Metal"}},
			{Key: "reusable", Label: "Reusable", Type: "boolean"},
		},
	},
	{
		Name: "Veterinary Services", Slug: "veterinary", Icon: "💉", Group: "service",
		Children: []string{"Vaccines", "Medications", "Veterinary Services", "Artificial Insemination", "Animal Health Products"},
		Attributes: []SeedAttribute{
			{Key: "species", Label: "Species", Type: "select", Options: []string{"Cattle", "Poultry", "Pigs", "Sheep", "Goats", "Multi-species"}},
			{Key: "prescription_required", Label: "Prescription Required", Type: "boolean"},
		},
	},
	{
		Name: "Irrigation Equipment", Slug: "irrigation", Icon: "💧", Group: "equipment",
		Children:   []string{},
		Attributes: []SeedAttribute{
			{Key: "irrigation_type", Label: "Irrigation Type", Type: "select", Options: []string{"Drip", "Sprinkler", "Flood", "Center Pivot", "Micro-sprinkler"}},
			{Key: "powered_by", Label: "Powered By", Type: "select", Options: []string{"Electric", "Solar", "Manual", "Engine"}},
		},
	},
}
