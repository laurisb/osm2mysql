package osm2mysql

type Node struct {
	ID   int64
	Lat  float64
	Lon  float64
	Tags map[string]string
}

type Way struct {
	ID      int64
	NodeIDs []int64
	Tags    map[string]string
}
