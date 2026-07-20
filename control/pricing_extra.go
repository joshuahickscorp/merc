package main

import "context"

type SeedHoneypot struct {
	InputRef    string // the seeded honeypot object key (always under "honeypots/...")
	KnownAnswer []byte // the measured known answer (may be nil for a class-blind seed)
	AnswerClass string // "engine|build_hash" the answer was produced under ("" = class-blind)
}

func (s *Store) AvailableSeedHoneypots(ctx context.Context, jobType, modelRef string, maxTokens uint32, limit int) ([]SeedHoneypot, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT input_ref, known_answer, COALESCE(answer_class,'')
		   FROM honeypots
		  WHERE job_type = $1
		    AND input_ref NOT LIKE 'jobs/%'
		    AND (answer_model IS NULL OR answer_model = $2)
		    AND (answer_min_max_tokens IS NULL OR answer_min_max_tokens <= $3)
		  ORDER BY created_at ASC
		  LIMIT $4`,
		jobType, modelRef, int64(maxTokens), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeedHoneypot
	for rows.Next() {
		var h SeedHoneypot
		if err := rows.Scan(&h.InputRef, &h.KnownAnswer, &h.AnswerClass); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) RegisterHoneypotAlias(ctx context.Context, jobType, opaqueRef string, knownAnswer []byte, answerClass string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO honeypots (job_type, input_ref, known_answer, answer_class)
		 SELECT $1, $2, $3, $4
		 WHERE NOT EXISTS (SELECT 1 FROM honeypots WHERE job_type=$1 AND input_ref=$2)`,
		jobType, opaqueRef, knownAnswer, answerClass)
	return err
}
