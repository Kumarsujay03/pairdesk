package crypto

// words is a 256-entry list used for 4-word fingerprint phrases (32 bits of the key hash).
// Chosen to be short, distinct and easy to say over a call.
var words = [256]string{
	"acid", "acre", "aged", "aide", "ajar", "akin", "aloe", "alps", "amid", "ammo", "ante", "apex", "aqua", "arch", "aria", "army",
	"atom", "aunt", "avid", "axis", "bail", "bait", "bald", "balm", "band", "bank", "barn", "bass", "bath", "bead", "beak", "beam",
	"bean", "bear", "beef", "bell", "belt", "bias", "bike", "bind", "bird", "bite", "blot", "blue", "blur", "boat", "bold", "bolt",
	"bond", "bone", "book", "boom", "boot", "bore", "boss", "bowl", "brew", "brim", "bulb", "bulk", "bump", "burn", "bush", "busy",
	"cafe", "cage", "cake", "calm", "camp", "cane", "cape", "card", "cart", "cash", "cast", "cave", "cell", "chef", "chip", "cite",
	"clam", "clay", "clip", "club", "clue", "coal", "coat", "code", "coil", "coin", "cold", "comb", "cone", "cook", "cope", "cord",
	"core", "cork", "corn", "cost", "crab", "crew", "crop", "crow", "cube", "cult", "curb", "cure", "curl", "cute", "dale", "dame",
	"damp", "dark", "dart", "dash", "dawn", "deal", "dean", "deck", "deed", "deep", "deer", "dent", "desk", "dial", "dice", "diet",
	"dine", "dirt", "disc", "dish", "dive", "dock", "dome", "dose", "dove", "down", "drag", "draw", "drip", "drop", "drum", "duck",
	"duel", "duke", "dune", "dusk", "dust", "duty", "each", "earl", "earn", "east", "echo", "edge", "edit", "eel", "else", "emit",
	"envy", "epic", "even", "evil", "exam", "exit", "face", "fact", "fade", "fair", "fake", "fall", "fame", "fang", "farm", "fast",
	"fate", "fawn", "fear", "feed", "feel", "fern", "file", "film", "find", "fine", "fire", "firm", "fish", "fist", "five", "flag",
	"flap", "flat", "flea", "flip", "flow", "foam", "foil", "fold", "folk", "fond", "font", "food", "fool", "foot", "ford", "fork",
	"form", "fort", "foul", "four", "frog", "from", "fuel", "full", "fume", "fund", "fuse", "gain", "gale", "game", "gang", "gate",
	"gaze", "gear", "germ", "gift", "gild", "girl", "give", "glad", "glow", "glue", "goal", "goat", "gold", "golf", "gown", "grab",
	"gray", "grid", "grim", "grin", "grip", "grow", "gulf", "gust", "hail", "hair", "half", "hall", "halt", "hand", "hang", "hare",
}
