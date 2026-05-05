package metrics

// defaultDurationBuckets describes the file-transfer duration distribution
// imgsync sees in production: small images finish in well under a second,
// large FTP transfers can stretch to minutes, and pathological retries can
// reach tens of minutes. The buckets are wider than promhttp's defaults so
// the histogram stays informative across that whole range without exploding
// series count.
var defaultDurationBuckets = []float64{
	0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800,
}
