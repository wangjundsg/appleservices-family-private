package dashboard

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"
)

type registrationIdentity struct {
	First    string `json:"first"`
	Last     string `json:"last"`
	Birthday string `json:"birthday"`
}

var registrationFirstNames = []string{
	"Aiden", "Alexander", "Benjamin", "Caleb", "Daniel", "Ethan", "Gabriel", "Henry",
	"Isaac", "Jackson", "Julian", "Leo", "Lucas", "Mason", "Nathan", "Oliver",
	"Amelia", "Audrey", "Charlotte", "Chloe", "Claire", "Eleanor", "Ella", "Emma",
	"Grace", "Hazel", "Isla", "Lily", "Maya", "Nora", "Ruby", "Sophie",
}

var registrationLastNames = []string{
	"Anderson", "Baker", "Bennett", "Brooks", "Campbell", "Carter", "Collins", "Cooper",
	"Edwards", "Evans", "Foster", "Green", "Hall", "Harris", "Hayes", "Howard",
	"Jenkins", "Kelly", "Martin", "Mitchell", "Morgan", "Parker", "Reed", "Roberts",
	"Scott", "Stewart", "Sullivan", "Turner", "Walker", "Ward", "Watson", "Wright",
}

func randomRegistrationInt(max int64) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func newRegistrationIdentity(now time.Time) (registrationIdentity, error) {
	first, err := randomRegistrationInt(int64(len(registrationFirstNames)))
	if err != nil {
		return registrationIdentity{}, err
	}
	last, err := randomRegistrationInt(int64(len(registrationLastNames)))
	if err != nil {
		return registrationIdentity{}, err
	}
	ageOffset, err := randomRegistrationInt(35) // 21..55; always safely older than 18.
	if err != nil {
		return registrationIdentity{}, err
	}
	monthOffset, err := randomRegistrationInt(12)
	if err != nil {
		return registrationIdentity{}, err
	}
	year, month := now.Year()-(21+ageOffset), time.Month(monthOffset+1)
	days := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	dayOffset, err := randomRegistrationInt(int64(days))
	if err != nil {
		return registrationIdentity{}, err
	}
	return registrationIdentity{
		First:    registrationFirstNames[first],
		Last:     registrationLastNames[last],
		Birthday: fmt.Sprintf("%04d-%02d-%02d", year, month, dayOffset+1),
	}, nil
}

func validRegistrationIdentity(identity registrationIdentity, now time.Time) bool {
	if strings.TrimSpace(identity.First) != identity.First || strings.TrimSpace(identity.Last) != identity.Last || identity.First == "" || identity.Last == "" || len(identity.First) > 40 || len(identity.Last) > 40 {
		return false
	}
	birthday, err := time.Parse("2006-01-02", identity.Birthday)
	cutoff := time.Date(now.Year()-18, now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return err == nil && birthday.Before(cutoff)
}
