// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package rules

import (
	"testing"
	"time"

	"github.com/m3db/m3metrics/policy"
	"github.com/m3db/m3x/time"

	"github.com/stretchr/testify/require"
)

func TestMatchResultHasExpired(t *testing.T) {
	r := NewMatchResult(1000, nil, nil)
	require.False(t, r.HasExpired(time.Unix(0, 0)))
	require.True(t, r.HasExpired(time.Unix(0, 1000)))
}

func TestMatchResult(t *testing.T) {
	var (
		testExpireAtNanos  = int64(67890)
		testResultMappings = policy.PoliciesList{
			policy.NewStagedPolicies(
				12345,
				false,
				[]policy.Policy{
					policy.NewPolicy(10*time.Second, xtime.Second, 12*time.Hour),
					policy.NewPolicy(time.Minute, xtime.Minute, 24*time.Hour),
					policy.NewPolicy(5*time.Minute, xtime.Minute, 48*time.Hour),
				},
			),
			policy.NewStagedPolicies(
				23456,
				true,
				[]policy.Policy{
					policy.NewPolicy(30*time.Second, xtime.Second, 10*time.Hour),
					policy.NewPolicy(2*time.Minute, xtime.Minute, 48*time.Hour),
				},
			),
		}
		testResultRollups = []RollupResult{
			{
				ID: b("rName1|rtagName1=rtagValue1,rtagName2=rtagValue2"),
				PoliciesList: policy.PoliciesList{
					policy.NewStagedPolicies(
						12345,
						false,
						[]policy.Policy{
							policy.NewPolicy(10*time.Second, xtime.Second, 12*time.Hour),
							policy.NewPolicy(time.Minute, xtime.Minute, 24*time.Hour),
							policy.NewPolicy(5*time.Minute, xtime.Minute, 48*time.Hour),
						},
					),
					policy.NewStagedPolicies(
						23456,
						false,
						[]policy.Policy{
							policy.NewPolicy(30*time.Second, xtime.Second, 10*time.Hour),
							policy.NewPolicy(2*time.Minute, xtime.Minute, 48*time.Hour),
						},
					),
				},
			},
			{
				ID:           b("rName2|rtagName1=rtagValue1"),
				PoliciesList: policy.PoliciesList{policy.NewStagedPolicies(12345, false, nil)},
			},
		}
	)

	inputs := []struct {
		matchAt          time.Time
		expectedMappings policy.PoliciesList
		expectedRollups  []RollupResult
	}{
		{
			matchAt:          time.Unix(0, 0),
			expectedMappings: testResultMappings,
			expectedRollups:  testResultRollups,
		},
		{
			matchAt:          time.Unix(0, 20000),
			expectedMappings: testResultMappings,
			expectedRollups:  testResultRollups,
		},
		{
			matchAt: time.Unix(0, 30000),
			expectedMappings: policy.PoliciesList{
				policy.NewStagedPolicies(
					23456,
					true,
					[]policy.Policy{
						policy.NewPolicy(30*time.Second, xtime.Second, 10*time.Hour),
						policy.NewPolicy(2*time.Minute, xtime.Minute, 48*time.Hour),
					},
				),
			},
			expectedRollups: []RollupResult{
				{
					ID: b("rName1|rtagName1=rtagValue1,rtagName2=rtagValue2"),
					PoliciesList: policy.PoliciesList{
						policy.NewStagedPolicies(
							23456,
							false,
							[]policy.Policy{
								policy.NewPolicy(30*time.Second, xtime.Second, 10*time.Hour),
								policy.NewPolicy(2*time.Minute, xtime.Minute, 48*time.Hour),
							},
						),
					},
				},
				{
					ID:           b("rName2|rtagName1=rtagValue1"),
					PoliciesList: policy.PoliciesList{policy.NewStagedPolicies(12345, false, nil)},
				},
			},
		},
	}

	res := NewMatchResult(testExpireAtNanos, testResultMappings, testResultRollups)
	for _, input := range inputs {
		require.Equal(t, input.expectedMappings, res.MappingsAt(input.matchAt))
		require.Equal(t, len(input.expectedRollups), res.NumRollups())
		for i := 0; i < len(input.expectedRollups); i++ {
			require.Equal(t, input.expectedRollups[i], res.RollupsAt(i, input.matchAt))
		}
	}
}
