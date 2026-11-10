package analysis

// Thresholds of the detectors. They were checked against recorded matches
// (testdata/opendota) and are the place to tune: change one, bump Version,
// and stored matches are re-analyzed.
const (
	// Lane report: compared at this minute against the lane opponent.
	LaneMinute = 10
	// A lane is won (or lost) when net worth differs by at least this much
	// and by at least LaneWinRatio of the opponent's net worth.
	LaneWinGold  = 500
	LaneWinRatio = 0.15

	// Turning point: the first minute from which one team led net worth by
	// TurningLead (25%) for TurningMinutes in a row.
	TurningLead    = 0.25
	TurningMinutes = 3

	// A death is traded when an enemy hero died within TradeWindow seconds
	// either side of it.
	TradeWindow = 10

	// A death streak is StreakDeaths deaths within StreakWindow seconds
	// without a kill of one's own in between.
	StreakDeaths = 3
	StreakWindow = 300

	// A buyback is wasted when the player dies again within BuybackWindow
	// seconds without a kill of their own.
	BuybackWindow = 60

	// Deaths at least this long are checked for buildings or Roshan lost
	// while the player was dead.
	LongDeath = 40

	// An observer ward lasts this long when its removal isn't logged.
	ObserverLifetime = 360

	// Awards need at least this much to be handed out.
	MinControlStuns = 10.0 // seconds
	MinHealing      = 1000

	// Approximate win chance: logistic of (gold lead + XPWeight * XP lead)
	// over WinScale + WinScalePerMinute * minute, so a lead counts for less
	// as games go on and totals grow.
	WinXPWeight       = 0.5
	WinScale          = 1500.0
	WinScalePerMinute = 250.0

	// Impact rating bands (0-100).
	RatingStandout = 70
	RatingSolid    = 55
	RatingAverage  = 40
)
