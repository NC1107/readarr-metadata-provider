package server

// iso639_3 maps the ISO 639-1 codes stored in the dataset to the
// bibliographic ISO 639-2 codes the Readarr contract carries (matching the
// code3 values upstream serves). Unknown codes fall through unchanged.
func iso639_3(code2 string) string {
	if c, ok := code2to3[code2]; ok {
		return c
	}
	return code2
}

var code2to3 = map[string]string{
	"aa": "aar", "ab": "abk", "af": "afr", "am": "amh", "ar": "ara",
	"as": "asm", "ay": "aym", "az": "aze", "ba": "bak", "be": "bel",
	"bg": "bul", "bn": "ben", "bo": "tib", "br": "bre", "bs": "bos",
	"ca": "cat", "co": "cos", "cs": "cze", "cy": "wel", "da": "dan",
	"de": "ger", "dz": "dzo", "el": "gre", "en": "eng", "eo": "epo",
	"es": "spa", "et": "est", "eu": "baq", "fa": "per", "fi": "fin",
	"fj": "fij", "fo": "fao", "fr": "fre", "fy": "fry", "ga": "gle",
	"gd": "gla", "gl": "glg", "gn": "grn", "gu": "guj", "ha": "hau",
	"he": "heb", "hi": "hin", "hr": "hrv", "ht": "hat", "hu": "hun",
	"hy": "arm", "id": "ind", "is": "ice", "it": "ita", "ja": "jpn",
	"jv": "jav", "ka": "geo", "kk": "kaz", "km": "khm", "kn": "kan",
	"ko": "kor", "ku": "kur", "ky": "kir", "la": "lat", "lb": "ltz",
	"lo": "lao", "lt": "lit", "lv": "lav", "mg": "mlg", "mi": "mao",
	"mk": "mac", "ml": "mal", "mn": "mon", "mr": "mar", "ms": "may",
	"mt": "mlt", "my": "bur", "ne": "nep", "nl": "dut", "no": "nor",
	"oc": "oci", "or": "ori", "pa": "pan", "pl": "pol", "ps": "pus",
	"pt": "por", "qu": "que", "ro": "rum", "ru": "rus", "rw": "kin",
	"sa": "san", "sd": "snd", "si": "sin", "sk": "slo", "sl": "slv",
	"so": "som", "sq": "alb", "sr": "srp", "sv": "swe", "sw": "swa",
	"ta": "tam", "te": "tel", "tg": "tgk", "th": "tha", "ti": "tir",
	"tk": "tuk", "tl": "tgl", "tr": "tur", "tt": "tat", "ug": "uig",
	"uk": "ukr", "ur": "urd", "uz": "uzb", "vi": "vie", "yi": "yid",
	"yo": "yor", "zh": "chi", "zu": "zul",
}
