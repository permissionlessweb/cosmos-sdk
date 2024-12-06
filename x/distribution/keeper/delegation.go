package keeper

import (
	"fmt"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// initialize starting info for a new delegation
func (k Keeper) initializeDelegation(ctx sdk.Context, val sdk.ValAddress, del sdk.AccAddress) {
	// period has already been incremented - we want to store the period ended by this delegation action
	previousPeriod := k.GetValidatorCurrentRewards(ctx, val).Period - 1

	// increment reference count for the period we're going to track
	k.incrementReferenceCount(ctx, val, previousPeriod)

	validator := k.stakingKeeper.Validator(ctx, val)
	delegation := k.stakingKeeper.Delegation(ctx, del, val)

	// calculate delegation stake in tokens
	// we don't store directly, so multiply delegation shares * (tokens per share)
	// note: necessary to truncate so we don't allow withdrawing more rewards than owed
	stake := validator.TokensFromSharesTruncated(delegation.GetShares())
	k.SetDelegatorStartingInfo(ctx, val, del, types.NewDelegatorStartingInfo(previousPeriod, stake, uint64(ctx.BlockHeight())))
}

// calculate the rewards accrued by a delegation between two periods
func (k Keeper) calculateDelegationRewardsBetween(ctx sdk.Context, val stakingtypes.ValidatorI,
	startingPeriod, endingPeriod uint64, stake sdk.Dec,
) (rewards sdk.DecCoins) {
	// sanity check
	if startingPeriod > endingPeriod {
		panic("startingPeriod cannot be greater than endingPeriod")
	}

	// sanity check
	if stake.IsNegative() {
		panic("stake should not be negative")
	}

	// return staking * (ending - starting)
	starting := k.GetValidatorHistoricalRewards(ctx, val.GetOperator(), startingPeriod)
	ending := k.GetValidatorHistoricalRewards(ctx, val.GetOperator(), endingPeriod)
	difference := ending.CumulativeRewardRatio.Sub(starting.CumulativeRewardRatio)
	if difference.IsAnyNegative() {
		panic("negative rewards should not be possible")
	}
	// note: necessary to truncate so we don't allow withdrawing more rewards than owed
	rewards = difference.MulDecTruncate(stake)
	return
}

// calculate the total rewards accrued by a delegation
func (k Keeper) CalculateDelegationRewards(ctx sdk.Context, val stakingtypes.ValidatorI, del stakingtypes.DelegationI, endingPeriod uint64) (rewards sdk.DecCoins) {
	// fetch starting info for delegation
	startingInfo := k.GetDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())

	if startingInfo.Height == uint64(ctx.BlockHeight()) {
		// started this height, no rewards yet
		return
	}

	startingPeriod := startingInfo.PreviousPeriod
	stake := startingInfo.Stake

	// Iterate through slashes and withdraw with calculated staking for
	// distribution periods. These period offsets are dependent on *when* slashes
	// happen - namely, in BeginBlock, after rewards are allocated...
	// Slashes which happened in the first block would have been before this
	// delegation existed, UNLESS they were slashes of a redelegation to this
	// validator which was itself slashed (from a fault committed by the
	// redelegation source validator) earlier in the same BeginBlock.
	startingHeight := startingInfo.Height
	// Slashes this block happened after reward allocation, but we have to account
	// for them for the stake sanity check below.
	endingHeight := uint64(ctx.BlockHeight())
	if endingHeight > startingHeight {
		k.IterateValidatorSlashEventsBetween(ctx, del.GetValidatorAddr(), startingHeight, endingHeight,
			func(height uint64, event types.ValidatorSlashEvent) (stop bool) {
				endingPeriod := event.ValidatorPeriod
				if endingPeriod > startingPeriod {
					rewards = rewards.Add(k.calculateDelegationRewardsBetween(ctx, val, startingPeriod, endingPeriod, stake)...)

					// Note: It is necessary to truncate so we don't allow withdrawing
					// more rewards than owed.
					stake = stake.MulTruncate(math.LegacyOneDec().Sub(event.Fraction))
					startingPeriod = endingPeriod
				}
				return false
			},
		)
	}

	// A total stake sanity check; Recalculated final stake should be less than or
	// equal to current stake here. We cannot use Equals because stake is truncated
	// when multiplied by slash fractions (see above). We could only use equals if
	// we had arbitrary-precision rationals.
	currentStake := val.TokensFromShares(del.GetShares())

	if stake.GT(currentStake) {
		// AccountI for rounding inconsistencies between:
		//
		//     currentStake: calculated as in staking with a single computation
		//     stake:        calculated as an accumulation of stake
		//                   calculations across validator's distribution periods
		//
		// These inconsistencies are due to differing order of operations which
		// will inevitably have different accumulated rounding and may lead to
		// the smallest decimal place being one greater in stake than
		// currentStake. When we calculated slashing by period, even if we
		// round down for each slash fraction, it's possible due to how much is
		// being rounded that we slash less when slashing by period instead of
		// for when we slash without periods. In other words, the single slash,
		// and the slashing by period could both be rounding down but the
		// slashing by period is simply rounding down less, thus making stake >
		// currentStake
		//
		// A small amount of this error is tolerated and corrected for,
		// however any greater amount should be considered a breach in expected
		// behaviour.
		marginOfErr := sdk.SmallestDec().MulInt64(3)
		if stake.LTE(currentStake.Add(marginOfErr)) {
			stake = currentStake
		} else {
			// load all delegations for delegator
			marginOfErr := currentStake.Mul(sdk.NewDecWithPrec(12, 3)) // 1.2%
			ok := k.CalculateRewardsForSlashedDelegators(ctx, val, del, currentStake, SLASHED_DELEGATORS)
			if ok && stake.LTE(currentStake.Add(marginOfErr)) {
				stake = currentStake
				fmt.Println("~ v018-patch applied, delegation existed for validator:", del.GetValidatorAddr())
			}
			panic(fmt.Sprintf("calculated final stake for delegator %s greater than current stake"+
				"\n\tfinal stake:\t%s"+
				"\n\tcurrent stake:\t%s",
				del.GetDelegatorAddr(), stake, currentStake))
		}
	}

	// calculate rewards for final period
	rewards = rewards.Add(k.calculateDelegationRewardsBetween(ctx, val, startingPeriod, endingPeriod, stake)...)
	return rewards
}
func (k Keeper) CalculateRewardsForSlashedDelegators(
	ctx sdk.Context,
	val stakingtypes.ValidatorI,
	del stakingtypes.DelegationI,
	currentStake math.LegacyDec,
	list []string,
) bool {
	valAddr := del.GetValidatorAddr().String()
	delAddr := del.GetDelegatorAddr().String()
	for _, sv := range SLASHED_VALIDATORS {
		if valAddr == sv {
			return true
		}
	}
	for _, sv := range list {
		if delAddr == sv {
			return true
		}
	}

	return false
}

// VO18 delegators impacted during v18 upgrade issue with slashing module
var SLASHED_DELEGATORS = []string{
	"bitsong1qkfkmwgc4k86k94gly3swjwfmqkqrxjzda2u7g",
	"bitsong1qh84y49mdccaux4tygwh8xpf6ukledxyr5gjll",
	"bitsong1ptl72763y9e9yakwxyzr22nrzdch4qmkzndsqf",
	"bitsong1z203u6vz5qec8t5wfmkmkx30a8nqp572kwujh2",
	"bitsong1rfhepmmglrk5lr3es48pydyayj8fdgjy9puvyy",
	"bitsong1yhepvjcaxgve7x8slmvrx0tjlr8nqpzdx4gkxl",
	"bitsong193nh6pz0mulg2k66kyugf6k7sslz323pzla6fq",
	"bitsong19j8dh02f5xu2kju5n07ppkwua76swdthtx4t8v",
	"bitsong19mmq66klqpcqjztdaclaf5tvknn4mjkdyjeqvu",
	"bitsong1xf6c3q0knq0k669hamfly2z7hveyzwqhvx7s7j",
	"bitsong1xtr8jq84cruu8scfuft04rpu7y58jd5ccs4e4a",
	"bitsong1x73sfl2zn0mcnzmfl5cuw09ngw05avzgwcaezg",
	"bitsong18z0slkkrh200u5avzjnjpa56qm5h29jehthgwx",
	"bitsong185yaxyzf5ejud04dcsslr606jalwramqrs6sny",
	"bitsong18kswfszx6n3wfw2mtsmpdyqh0nyzzqafa020sy",
	"bitsong18euwex79grh0vppryv9yc9d2a35p9ynhdsehrh",
	"bitsong186hzu683fe2zzj8n03nwyh8fw3jt7dc6d9ws6y",
	"bitsong1gk3m80amvc23h0khytkx4m9nauay4ca7gu54cl",
	"bitsong1fpxjtfc3dnf5ls3qrjxh6damyguqf23vzd93cm",
	"bitsong1f2hm9enzga55fn3mg80kc9pa2k5lkuyv7t6h43",
	"bitsong1fd84rfq58wdm4t2gp6pwj6xrcv7rlljn08gcjk",
	"bitsong12zt7tz67hr2zjfa3h87ddl26w92x0pam4c07h5",
	"bitsong12rfwshjk3ucazufjt6xthkc3psnqqkufnqpwt8",
	"bitsong12x0kme00ngjpypcp28jlrzy6tew8p6htklf0ec",
	"bitsong12d43zv5jxct5vef6nmmuu205tsslg4t0ze8805",
	"bitsong1tgzday8yewn8n5j0prgsc9t5r3gg2cwnyf9jlv",
	"bitsong1tn0xzu0msj6nhs5n43uet87ljzsrxgy5wgmwdg",
	"bitsong1t6gznlwflkj0vq2jwj8drgjmg7dhwlrv92jgfx",
	"bitsong1tm73uus7ta3y30efy8n7rggdrgqytjg8jqqqut",
	"bitsong1vzwgw9x4kynpsgtt6l9wtu7me4pslv6gwlra86",
	"bitsong1vxsaq3dqsekfv7k802kdjt399w024qh3le5qla",
	"bitsong1v0rrj0wea2tgje8nmvhv3cka49jf2z4qs0uqxw",
	"bitsong1d2jq4vrvpxgsqeqd7c60ys9z3ufcsrkvg2g66r",
	"bitsong1duwalq9s277gjuqsy6hzsqckn84qce865eyc4x",
	"bitsong1dazpyy9t32w2hhfqkptulsje3esavkphap98ef",
	"bitsong1da4vt32g7a8vpcn7yy36s0c5u43ufqrxvnteas",
	"bitsong1wfd8n3vmch7vfwx70upd3ypspq49rjgh0m546h",
	"bitsong1w2x0wz8at6ccjkhyxslkgs3383gj03r07grju5",
	"bitsong1wdmzwy68zukkdztvuskat7a8vy5cmjatfmyvc9",
	"bitsong1wnawzxdputr3sy0h5gxj0ntyxfepghxlpgl32u",
	"bitsong1wkzcxwcv7ga9sr79px47atwtfmsha767kcktjv",
	"bitsong10z74ulznt9ygevkqptpu6sjv8gf94cqhvnf4m2",
	"bitsong109rpfwzcfgg72ulwerfcawqjl9w6uajccx8ryy",
	"bitsong103ye2t6ccrjfs2p5hzg75vj8q0pq60qc92e2d9",
	"bitsong1shuy0sp9j5tchqekzug57afpe2ln3zvyvxzej0",
	"bitsong1smz57saknkuleszux3ghx7hqlwm9dz2llsqaw8",
	"bitsong1sl7xnxqt5y0asredh93ajfy8kynqn42sptmh6t",
	"bitsong13pspx6xslv0qdfgt33fhcr2szmt6vc5fcwdlry",
	"bitsong13p5dt20sggkxpj9w0lgenk87tuj8ykz0jhh8e0",
	"bitsong13r64uaav7tqzceqhr8ug3whn89qkcqm8ghlq0v",
	"bitsong139yus885rzkwx4p33fvqkpjvfq23syryyz20mc",
	"bitsong13fng3303v37mz4kx3d6gspuutf0sqj6xrfhvvl",
	"bitsong13t7aasuqn5x6e3rwh86udgcdt2v7tkj68w2hw7",
	"bitsong1352qm92l6p3w4jldzd66yl9xl4c84zed00ccpt",
	"bitsong135w80y7r63m24p6dhgnx9mp9jyupt7luswmnwl",
	"bitsong1nttz2z4nd2d806svu5mu99ppcxdmmguw07hswc",
	"bitsong1n3qsdth05w6sugd5u4kmu8mpmn5saqe2kvx6ge",
	"bitsong15f46k58ca87cj7844rgz4z47axpksq9659tsf7",
	"bitsong15s86cfk8effmf5frquc0m9jrqp4p37qnhfaz9g",
	"bitsong15cqln2p0yvuxvn3n7mlvcqdjc06mlsdzzhldmx",
	"bitsong156raqq0fsj7y3d5hygdvgz5zzl0qekp09wkddj",
	"bitsong14pz6exydwq85hwfl8qdkgdh3fer83tgyuqpawr",
	"bitsong14rct98qvjh36xmard8w7rmqnyfr4vrvl7qmmhn",
	"bitsong14yt3zx9dgmlpv000jyky8ru05vjwwhfy22flht",
	"bitsong149knztadmv20avq094c7j3yq6jeq0yarsd95vs",
	"bitsong148c2zqnjtzx6n0m6fnytmtdhswdk7s3ndk683f",
	"bitsong145gfj3entp28sa7x99e8welm3hrfjc6v7qysnl",
	"bitsong147gugaen33drfg903tudn8ktafzcd6cf7whdaz",
	"bitsong1krs4aw4dg33az4zda6ewm6k24fxp9mv3r05k0a",
	"bitsong1k23le0a99t0s9tkwk4p5d624x4fsyhnx2ttmxj",
	"bitsong1k5x79gsafyltus7r8nypvz3tyug97756x62fyz",
	"bitsong1hrtslzkl28l6u9e2j643xq476k92t5ev7nd045",
	"bitsong1htz2dqtjskwznylt4a2t37gwhw9g5ktqjzj4cp",
	"bitsong1h30732dujnpxz2lvwt8avprjlgw4uxvhj9sff4",
	"bitsong1c5d3j457hjdcprz8zel48n53p8tzs98gjs8q4x",
	"bitsong1cknd5algcc66ujwce0x6k0wmzvf8zja2ck8z4q",
	"bitsong1exqx726t7jm3k3z20l94e03gxsf7gsvacdk04d",
	"bitsong16heynh5egr3ep53vflxl87sj943s08jxjeyccc",
	"bitsong167jkmlyh6r5dq226750chzyyxe98nr0lq8g2eh",
	"bitsong1mx3y7vezgseqql4z4k4xkt8jch0xll2qzu9uz2",
	"bitsong1m5ky3shs7a7nj0nz4rhme09xzmjfw62lcsrnvc",
	"bitsong1uq3glwdf26z7qu72j2gfuvn8sk23tvqxpdujxw",
	"bitsong1uy3q3ukt0pp9ctkznmsjcxfq0nmqf25l9ps3js",
	"bitsong1utj8nycuss6wkncm732p2vs7z6sakelmnq3m30",
	"bitsong1a8pww9snte8qgtsq0s03ajnsqjhhuwxkttp97j",
	"bitsong1awualr4stlwspxce2nlpnjzwe3z3naklyfc4e0",
	"bitsong1a3sjt4k2lclhrhghav2meuqusvzkn4krsrm7gl",
	"bitsong1a3e3g8d9eanjjmtc7l7rsj3gar0h7l3pv8aumg",
	"bitsong1a5mn8x9kp9tft7ucfgaj2mm72fpk0mf9ktjvna",
	"bitsong17sdgajat4k7hu80tg3q4vhdywealsuhqlxgz47",
	"bitsong174nx4hx9882qfkvn74lgf6fmer4vw6qt2m7eas",
	"bitsong1lgx382xtadrwjc46vz8mdm9l0nskyelfq7524j",
	"bitsong1lvtuxshezha4kyry3gv52yxf54nz0qn9lft2a6",
	"bitsong1l5fl0x0mz0euezyyww846ufs4znlw9cywrxk44",
	"bitsong1f42l7eqzhpsgd4jxksd26ce2rg8q57yqsafntz",
	"bitsong12v3z2eyzhhdlrnshh8edykatwg8lxe2usfwqve",
	"bitsong1j98m4tzhzktqgwmmd3q8k9trgch5ssxnqlz2pt",
	"bitsong1nphhydjshzjevd03afzlce0xnlrnsm27hy9hgd",
	"bitsong145gfj3entp28sa7x99e8welm3hrfjc6v7qysnl",
	"bitsong16qzjv0n6xzxaczgkepn2ladfx998tcgtu6dhu2",
	"bitsong1t469s6x5sdlsz8wzjs79tdh9y7u8f3znk9vjqr",
	"bitsong1hrtslzkl28l6u9e2j643xq476k92t5ev7nd045",
	"bitsong1a5mn8x9kp9tft7ucfgaj2mm72fpk0mf9ktjvna",
	"bitsong1l2kthmf0gzlmscca859zs6fa22p769phs9hpjx",
	"bitsong1qxw4fjged2xve8ez7nu779tm8ejw92rvwgy4s7",
	"bitsong1q8c6ktzkfhm0l0hr9q4xamzur8y6n5m3ma52ra",
	"bitsong1qtrm4jrfmtx2rgdet09shezuvh0vv5v50ztr3k",
	"bitsong1qn0d8m5s8k5e6ym5lu05g7xaj2zf8y5779jvvz",
	"bitsong1q4j02tlfchjl626kwynkk3kahzm53ap3qef4kv",
	"bitsong1q4mlgureap9kg025t8c26au64gmmal5f2ftt5p",
	"bitsong1qh0hpxc944x576pulh502wsj2atfa328xklwr8",
	"bitsong1qh66v4nw90q93nyre8mnn9gml07my78dj4hfnw",
	"bitsong1pzdkhzzw2kflnkd2rjcdl2placrr2v0kch3wgq",
	"bitsong1p8tqtnyh0wqnknkfs3cdwxsyxur0thlvgreec6",
	"bitsong1p2cjtehcqkzy4qd5hpnuqlfvcpc7pt5sp5xveq",
	"bitsong1psedmd8y0uuhk5cjau5sdfuv39mde62xwlk7s5",
	"bitsong1p3z4l0gcnr7yywxcuk5hccq64srpqajwvg7g9z",
	"bitsong1p346eglt5cjy5tykkfmmtw8jrq4jc5lt8yzzgy",
	"bitsong1p4mwu30ehp4aquhx96kc5c53lulwg3mgh2mv44",
	"bitsong1penglcm6kls4qxa6pac8t8ntjagugcezg83n5h",
	"bitsong1zp8yr55tgmcnduzqvjydmwtm898d9v5zrfmpr5",
	"bitsong1zpcf3y4taxgmljzk29jqvyjwamqhgml5a5cqtd",
	"bitsong1zv54p39x8t5pshsd5p03jf8w62865nd862rlzn",
	"bitsong1z09uy2f2y39pjwdhk3a457qs0pnlhpna8tg58f",
	"bitsong1znmpelq8ehgmtkw45gxxce788wc88hc4ttml3t",
	"bitsong1z4cveys6e8983ktq8m3hjdw6a8kckp24uvm8l0",
	"bitsong1z4m5ec3qjwdyz3dl33fr0wp9l5p3gpaj734fgu",
	"bitsong1rgjymkt9usutasg2c3qlpk0tvdpp4gjdma3rq9",
	"bitsong1rv98uhqtk00j90ngeqm7wfvrcxdxrzpl7mf6u7",
	"bitsong1rv088x5836zal7sfxddr5905eutauwrchpdnjd",
	"bitsong1rv62alwx580v7rdasxvz3xkepzy6wnfa62yakt",
	"bitsong1rns4tg39ses696js09w7tvp33m54dfdp7awf8u",
	"bitsong1r5y8a6uznm86qmnnlppg6huyyaejwumt95gw3w",
	"bitsong1rkp99xxy8eg05v5l0uvwgggsp543te6x7ee8nr",
	"bitsong1rhaur2gydse28gtrrd2u2zv4703je07yfwtpzs",
	"bitsong1ru9346jkcy762neudmnvuswpnl8jmkteele68e",
	"bitsong1r7mul5yxz4e44dlxm0s5r894g3y2tkkqvl6jap",
	"bitsong1y8g20hyqf4etrg2m7rjapjvh3vgqqvv97yjve6",
	"bitsong1y2gsual9m6a3twsc662f0fnfcm7awsrnx8jtnx",
	"bitsong1ytr0nujljr44t7kw2vhe566ecjz8mtn8lr7kyt",
	"bitsong1ytamc4wp4n7kel8wmzz5ct0r6sn6uaa67xw0nh",
	"bitsong1ycp7n6xpj32gwl8an5629kmr3n9vzt0x98e5zj",
	"bitsong1yujx9nl79802k3qpg5pt60ycqyk8uqdnfljsfl",
	"bitsong19zw69298c6mj9prpdyxv2yvy0ge2az6mle5z4c",
	"bitsong199ndqufcfn4h3vh4gl6da0l6u8ct9sclgzzzru",
	"bitsong196vfs08w7fskdkgd46pad2dummn3gr26zae4z3",
	"bitsong19aqssphm5atcmcxlmnd3x7hwdwmtqk534ywcaz",
	"bitsong1x8a3zt2xkv7l0unk0ewv7n5k5uhhcj4dagspjh",
	"bitsong1xtr8jq84cruu8scfuft04rpu7y58jd5ccs4e4a",
	"bitsong1xwfy2c7u5wyw465xmzm5gcl5tg3kgyefk7fle7",
	"bitsong1x4ucdalf8evnfayzdlgus4jsmrkgls6dug9j9y",
	"bitsong1xktcxm97styu2k3p4pr2qrxygyq7cumypj0mxr",
	"bitsong1xmu7k5l5yylxhfqfya6ecd9y9f29w8yc789564",
	"bitsong18p9n626yfllgkmakxf4wrcue7d9elg7m6p76vf",
	"bitsong18xr5akchxcapcjff8d578tjge94yx4rtry5den",
	"bitsong18gtx47dv03cxrg5s4vuwsk36rvjyudcj4xvdv8",
	"bitsong18tksw5paeg4nt08gp04yy7gz97yhquy6tyalwn",
	"bitsong183gjemkc4ladqunhhusr82lm9gx2pw3ynq7qx7",
	"bitsong18n3vlcthxv5s2s4zgjdrjr5a6lw9e7tmthnwnk",
	"bitsong18uphvaqkuskny8khtc95mwq4nufmspy7agenvl",
	"bitsong18akd8t6y7ce56elpxs77vg09y6na76ueyfcr37",
	"bitsong1gpw7zf88eqv43pm97twhhwd2a6w4nhtgc5gzm9",
	"bitsong1gpks5g0ewvmyfa9tqge64m3lh9pn4q6ly7d339",
	"bitsong1gy39m68rz3rlplpyuqsm0qygcp0pq2axp89x4j",
	"bitsong1g2px5vcmu9w0wxjgs57u43673av653dvp9nz49",
	"bitsong1gjqa3ne6ntfd9hl55t7zhhmhv9pytj3ahytv3n",
	"bitsong1gjsz2c2zh3l22yysmmxexyk42v943whzjyrzvc",
	"bitsong1f0urc2u0dd3ne6yl28armthxkfhz39sznl50fr",
	"bitsong1f5ze3svwg8fgjuwwnr743j6fr9vtyr58nex7tu",
	"bitsong1f4e2hrap79k9m4hxxx7t7c25szxx2zplc3d7f6",
	"bitsong1fedmq0zkjh8yermnxeg2tfqngv96stu85gg609",
	"bitsong1f6zrtf4t7esuvq5vhevvs824s4j97dlvcfcfv6",
	"bitsong1fl4u3qkswjzfwt8k67l7f8ja9lwk5zwe3sly7e",
	"bitsong12fxe6mqcvh3a3w4kwurt8j32mftajqypl8xf76",
	"bitsong1228uj685uynu875r2tlwjnkg52y7etqy287t2h",
	"bitsong12v3z2eyzhhdlrnshh8edykatwg8lxe2usfwqve",
	"bitsong12e7ptnnn9cp48axz0s2wh5suumh8zfu4z55k8j",
	"bitsong12av285uyxf422gtpfsfl6pszy75j3cmc75ly6r",
	"bitsong1tqah2rtjj2efvay87mtc60539qagm34tne36h5",
	"bitsong1txge2e6dmr2fhz89f3amds893ry9jmgp7cs770",
	"bitsong1txa4dnd5v9y89w5jwerxcpfsvsz49vxx6m3s2y",
	"bitsong1tgzday8yewn8n5j0prgsc9t5r3gg2cwnyf9jlv",
	"bitsong1twxmj9mchnpxt8f3ypet70yg5k4xq59cf5fkw5",
	"bitsong1t4ky9w6vsxu3j9w3mauq3p0d649ehklnpcg5fq",
	"bitsong1thd0nj84gfnuk460yetcks2qlga0gn3apn6lxz",
	"bitsong1tm7y05ug26u4twjsv7el5gn690syzzhchsqtu7",
	"bitsong1t7s8pdh2lzzvyhpvqzhalgqhg75gpfq9rnj24z",
	"bitsong1v8jywvzpjezaj4k8g08dmk8yczk96k722r7sxy",
	"bitsong1v2g8jy626nrzr4spy7elhuhcwhlr9yn04ctm5p",
	"bitsong1v3e9hjaz5uu8d4hn3u9f5fpn5zn9pxyvtstaag",
	"bitsong1v5rcdds8k2vhnk845k30vcays6k6ad7x8qpvpr",
	"bitsong1v4k4mm0z05m0am8cy25tj0gwr0g3jatydftrjm",
	"bitsong1vkqcy6sj64qd3mq0wwza5j38qcnqwcxu5zdkc3",
	"bitsong1veh7fqecq5hrhxddpvdt24fh6jkn803tc43h5f",
	"bitsong1v6umul8nun2xaml2y24zrhwc3gx2lv389hrrp6",
	"bitsong1dgfdmpz0hae6h5dpaxz4t9p5r30lnfk969q53h",
	"bitsong1ddn5krvfu3fm423z5mklftqqce49ywtqsa8djq",
	"bitsong1dwdalwyrr24zjexecgf2zf707a87m6qg4t7mzr",
	"bitsong1dsz73xm52f7z5er0fucpx7ynx3kp4qakhrp5sl",
	"bitsong1d39zacyzfscqs25zws5auq9petzhf9f9esqr0l",
	"bitsong1djytet30fzdf8cq0tm7wx8m8kw8qfzuw7my2hs",
	"bitsong1dceuqqcqyhrlamffg9cgj0a9wx06h2c5jmtx09",
	"bitsong1dujwhlput2neewzxryrfx7ljsg5y8srt3kmy4q",
	"bitsong1wrppvrukyqn3xm038r7gsjc697yw34xlkehpts",
	"bitsong1w8t7r2y48mmsdzd08ad5cc06x4lkcqy9y3vr2k",
	"bitsong1wv8s3cflrukukpe8wt0t246ewamshqlnm4mjvw",
	"bitsong1wj860r30rn6vyy95n4lq40z49d3yhltktq8zjk",
	"bitsong1wjdx6p90zsetey59t0rxeaqj3632hr9ugc5prk",
	"bitsong1wnwwltj8xynmmres4mp0uvtm2xjzzjf8qx38fl",
	"bitsong1wkzcxwcv7ga9sr79px47atwtfmsha767kcktjv",
	"bitsong1we33fhr35vftksk35ygegm3ztt0tgllhte2znu",
	"bitsong1wlureh2j6exueae8hlp5v67uedjvesjrp2w86h",
	"bitsong109n3mplxjrrc56k387zy6frxfsznu4rakm266e",
	"bitsong10wrjthp576ymgwycrp37cuf0v0y8yf5penhvls",
	"bitsong10wu2vcpe7nven4udxvmwn3pz5cl4c9hr3fe438",
	"bitsong100lkecv7vgng0e5au37zj8jkf76n4zk6rqgje0",
	"bitsong10swtxt9hcmu3ygjey7fd8m36jq7mvveafjvey7",
	"bitsong10nn9fc4zw0kydxz4s9jqceq4mf7g8a7whs4rzt",
	"bitsong1046mvez8fmyc7p3hdcjsm4an28j0lxcr2h0jd2",
	"bitsong10u02g3xa6dgkqelelf955g2vhj3r36n7ca0czl",
	"bitsong10ay2t95pqfme6rhma7gs7q6c064as9g3t6zm57",
	"bitsong10la2t6yaft4ql64gmzu0ud3tzx7s0emv6tr2gr",
	"bitsong1sq9egj50lpvph7er22emu7hyxvy4w68xkg66cv",
	"bitsong1sz634cwwrl2uzy8d9055zsguzm65jn6wv8qgf3",
	"bitsong1sxsk5v09znwuhux2scueyspulyeypdnwm7v8fw",
	"bitsong1s5ppszxf5l5tvg69t7q8kpm4h7nxw42a9my658",
	"bitsong1s43d82z8l7u5nlv8j93h7ems8l6nwxsmh9k6xh",
	"bitsong1sks8zmfhz4klucvn2d9nsyc2u252nzel4sqxz3",
	"bitsong1sc9h4dlftwqv82utj52xfgrv0xevuwng3yekfd",
	"bitsong1sel57ncl582tjux64rh9ekmrat4y8wunvmzgnv",
	"bitsong1s6ypwqfggvuc48j5ywj8gupsel5t3tvdzm7kjm",
	"bitsong1352qm92l6p3w4jldzd66yl9xl4c84zed00ccpt",
	"bitsong13elcgng9x2cdgprx498lvyspjf4kd2rjqjrw3t",
	"bitsong1367qqvacwa5s9f0hrq33ntfqky5usmf6rkhqcs",
	"bitsong13uup3md785lsd74aws4hx2et052mferwrpmufk",
	"bitsong1jgyvfjfghruheeyp2ts04hd03ztydt792234ck",
	"bitsong1jfrs6r5q9shj0w27ydm3gn86pa44fh9an0ngay",
	"bitsong1jeynxcsuafyycyjldsfyyfua6lh7crxsswzes4",
	"bitsong1j6z0tfj4x20fsszwef4jwfl58gu5tlthwqam02",
	"bitsong1j7x0gtvjczvzhuspka89lyu46gt2nlxcs6e672",
	"bitsong1nphhydjshzjevd03afzlce0xnlrnsm27hy9hgd",
	"bitsong1nt0lftj06203ay3drdqzvsyfv5mzreknhv5nlx",
	"bitsong1nnvthr34cyhek5cpxf0l7j6ay4qd5ya9gfq2ej",
	"bitsong1ncck9qvhfkvu9edreqm7w594xafjh2ume633dk",
	"bitsong1nah2l3d3xe86mf4cne7wty05y23ykfa7l2lau2",
	"bitsong1592j8mgej30s8hqjaq48np0m9h5jaw208kjqse",
	"bitsong15fztdcdfy9p0jwl4q7m2624ywhncmae5w7serm",
	"bitsong15t2cqct43c5dl6205qey97zjsa6qph00ujw227",
	"bitsong15vdxv2lre937p5rm4xg4uldpf4u4y9ulz4d2cq",
	"bitsong15jc2hdsyc4glj3kaet3k40lhrqxe7gqx9jpwwc",
	"bitsong157z90gd97m5cs2sky7ddenvuvnjr7xyp5fz5zs",
	"bitsong14vgp7l8ap53tlp0kg2gk259e9lmyza8e9475yu",
	"bitsong14wscvujhp76z3yx3avaz6ysersh34n7ydzee80",
	"bitsong143g63qhy9u85ysvq9h2mvmntucda8p6fwqjpje",
	"bitsong143et2ftl0mlpmj85wa7ckr056h3wqrf043n7er",
	"bitsong14ecpdt66xxceapx3m4p5uq5amgnjq0vs2lpwrn",
	"bitsong1kr9phxmf5efd7z0mf3nmr6k8qt7u8pwvlcxvg5",
	"bitsong1kr8za0qkzzdpv98wk9ghq5em80850pe2x7365f",
	"bitsong1krs9xakfe9fyrgpzn0vlu85n6z4vkvsl7hcjau",
	"bitsong1ky4jdsnmh5eelds0aqul5zgnpsf65agkzmfatp",
	"bitsong1ktk9zfwey8tczpd2delusg2nppy42pxth32726",
	"bitsong1khtuam45r22tawvw0hwrvz4sky0497zsz6qw9q",
	"bitsong1khj3q2aelhuwcnj9j65pj9g9772qqv79pp9yw9",
	"bitsong1k69nerm64znzlet3qjq2x9926xma2dr79r7j8q",
	"bitsong1k6wue2euufg5eelep8ju2fz9ytu2yyscexwlvh",
	"bitsong1k652lq822mnvu9furfmmq3wsmufvvyg9pzljrq",
	"bitsong1hzw5yyl3cwcu99vua3sem9ve2ufn408pqmnh33",
	"bitsong1hgj3a9jxxs3vp78su7qt9j9h0jmm40jgzqsd78",
	"bitsong1htj9wpwcqrtkjeekm0pzmn2s5pp9r7xj4vmmqr",
	"bitsong1hvgwl3pzflkqwuhmxf46dje6ena96x74w5km4m",
	"bitsong1hdsmz2kqftgk2ypa5t62aauvl0y3c2nfx79zhy",
	"bitsong1hwdnref3hdapkjqx885u3la0ktt4x3k6l358ad",
	"bitsong1hkf6nmeuvtz7cjgqm3658mvkyfwjrh5f6m4x7d",
	"bitsong1hujwqwxckwqxvx2rwgnsyrzumke05uhkq76t90",
	"bitsong1h7rk7kfke9ll4av8jqj9y50dp27z6hlwhpysp3",
	"bitsong1c9lfrqwtw2lznx73r855rflwn97l9gexyzhp3e",
	"bitsong1c3zdp3tuwrzpfz6lr886qhr3gl7pfcngtw8503",
	"bitsong1c35ad98af8tnygtcplgak22dqpupl0rurj7xx6",
	"bitsong1cea85jch64s625g0ng4cdvrqhgyzzexqahqztw",
	"bitsong1c7mdjdqa76ynjyer0fufu3k2z7snyql8675u0h",
	"bitsong1eps38tztsyq4wmts4ca8x6gm0w9d53zja0mrd7",
	"bitsong1ep3sry7cshyrl20mvleepnnnyqnkxjpltnjh9q",
	"bitsong1eyu7a2uahgdp9jlm5vulu5hs0xcyp4wzme2vff",
	"bitsong1e9chsrhf7xv2h020etjx89g3t63ufqq05pcvlp",
	"bitsong1e2pxujqvad65zhshxadd6hcpq996xtp0v8lkqx",
	"bitsong1edttrpjjscqgr244f43l5rq0yjz04hcxgty947",
	"bitsong1ed3gvk7ua63d53xs0lc93es45j85qeyqce97w7",
	"bitsong1e0g45llyd7vfxldhrsd5kd8pfh84yg9zew4l0s",
	"bitsong16qzjv0n6xzxaczgkepn2ladfx998tcgtu6dhu2",
	"bitsong16r6dtumgfpk54sl98cp57s3jzn59pfxcx3w69j",
	"bitsong1695cm5045995zsdfyfkqavhyed6ekp4dpeffj9",
	"bitsong16xvhxavg0ecsa0hmpcmn2r8yv3eza4cz5pr9f3",
	"bitsong168ut9j98qm9fldn5q6f4kujphy0w5j0ad0hksj",
	"bitsong16twepy2ztp2xgaqsdd6c7w8h59hds5uc5nxq3q",
	"bitsong16v493zyshxw074n6efmk0uy2uydy8pycta4yg0",
	"bitsong16jlqxlelp42c77d3mg9njf99gdwcjdesf6cdja",
	"bitsong16nsxy6mwevx2lj8z8jfrphy0x4d9zpc8gh9csk",
	"bitsong16n3qhdawxcy8capj3ej80sgzk0t627ss3rkp4w",
	"bitsong16cyrapn4udh03r54mznsem8z42vhdksxq6cv2g",
	"bitsong16ud238wmvumdv84mkvvvnrc6n0l9apzyzsh9e2",
	"bitsong167k7s2ttazvv7jh55l9fhc5ts0f2jsxd9pj2ut",
	"bitsong1mrjmvy0sswem24vqlk23lvcamlj3n2yvpuetmn",
	"bitsong1mymyafaugh6uwjumwx42exkuuzq6pmv83u3et7",
	"bitsong1mt09wpwh4yjwmgr893yyx9m9f388aaj7hs6v62",
	"bitsong1mhyr2rgd2au6d9h3t8are69hatxuzrg3twn9ta",
	"bitsong1uytvmfsgxfks7drx2m7s92unmgt7yck9qdtv39",
	"bitsong1uwpk44vqrhaa9pflvuf0e7pqvyzusdxrvnnmwv",
	"bitsong1uaw20nkce9w306r3etnk0j3qzrggwna99uxegt",
	"bitsong1ule5nwcc5l29rzctamu8kh7cns246v4nhnsdk8",
	"bitsong1apa0nlaq780dylcvajs8zaak550tjfnk8vlh4w",
	"bitsong1azpm7xl3vfj85jnk3dlw334469p7c0as9430ld",
	"bitsong1ayy8knfdhepp2rws2m8kq5jzjm67fsylw0lamu",
	"bitsong1axpeh0dh06942tkcpyg526pkqz80kzcfg6kjfd",
	"bitsong1ag0vj20uwlvczh54lp7adxgfpjf0yc0ul4ls83",
	"bitsong1ad2ysetvyzhtkdlly5ekm6pg7mj2h2c73jrnv0",
	"bitsong1a3le3ndge2d85qayrhv6xrr4uwvdsyyfe3tjss",
	"bitsong1a5rpx45zhcejs7jz4qhncrf0gyuhmla42eujdn",
	"bitsong1a5w6p5qcevadfuy6n6pgulmhml7yff85zd9fuk",
	"bitsong1a5sgk8xwua0l8qkjpcl864pfnp0kutk4usqvct",
	"bitsong1akz5gzyvg7llu7vx3xa7eyyygyyj0mlvsrwa2n",
	"bitsong1alfwvnzkzn063pr2p3dnw5anfh7amrv0u86lh7",
	"bitsong1all77ugzkmf2yyg392cv67k6kw70m5pywgqaan",
	"bitsong1782tz5eap309nwy0y8eyyc02njcqntrhzk9glm",
	"bitsong17sdgajat4k7hu80tg3q4vhdywealsuhqlxgz47",
	"bitsong176kzk5zzkzm39allhwjf2mye6fdn09e3qa7q23",
	"bitsong17ahrafnurfkqq08n635gnwj58dzndl5tcl0khl",
	"bitsong1lrzynykmx9scr394nsqumwmnq6appywzk4p908",
	"bitsong1l886u8952svqn5jh69j6nqhj2lsea2fg8rd26f",
	"bitsong1l30kphesg5a7nj9v30ygpy5f94gu05vyk7vt57",
	"bitsong1l3cur6rcxdjwl304mc5tv827pgyncgjq24h9wu",
	"bitsong1l5skpzf4ypsn2062j0lr3kaj3asg8uvg223dpq",
	"bitsong1lkszs2qrkuep9nkq8tupg97qmf80ef0pwxp2t4",
	"bitsong1l68eyxna0c87mm3uhgyxdvln2d35gg5fwc0ezh",
	"bitsong1lasztz9jnvlhma8fcl9ttrycaye6adqd8lz5ju",
	"bitsong1yxchl7wqeleevxz9k09xcmc2pgtc0amckdk33m",
	"bitsong12v3z2eyzhhdlrnshh8edykatwg8lxe2usfwqve",
	"bitsong1tgzday8yewn8n5j0prgsc9t5r3gg2cwnyf9jlv",
	"bitsong1vqj8du37hwzdc0eyfpews66n65a993xmshssr8",
	"bitsong1v466lxttkhgsey7m4lfznmh3gns7sfvv4q0jxn",
	"bitsong1slnkc2a8lhxgz5cc7lg9zlgzfedfpdvew8tr38",
	"bitsong13t4pa894eduaryhgnfadv92l2narzfdwnvgnfh",
	"bitsong145gfj3entp28sa7x99e8welm3hrfjc6v7qysnl",
	"bitsong16qzjv0n6xzxaczgkepn2ladfx998tcgtu6dhu2",
	"bitsong166d42nyufxrh3jps5wx3egdkmvvg7jl6k33yut",
	"bitsong1l0fp9527ylrzx3e5986pqq0pnw5vg7ujyttvgn",
	"bitsong1fv5tyr7uswq0j55vcm8zvznqrdc24pahd89qaj",
	"bitsong1tt5cwm23wpfvmuqvnwue3us6np3l07p423fnz4",
	"bitsong1wusnupm08xwe05zgvk6frqjuxak6q5anfswg3t",
	"bitsong13g02l4y2es8ppls7ar5zh58ldre8q7tsd72ff7",
	"bitsong1nphhydjshzjevd03afzlce0xnlrnsm27hy9hgd",
	"bitsong14xtfqyc3x8z9r6mqw6equtu70t3ftl2kf6v30d",
	"bitsong145gfj3entp28sa7x99e8welm3hrfjc6v7qysnl",
	"bitsong1zld9kng3v8zztaawuhkkv70jc49xewpgryva8z",
	"bitsong1rmxh22x8yajysf0hz7379kgszfnmhj9ae77pas",
	"bitsong1ynj2u9x0pgq6gx38pllwrg7948l9yp9lztgtgg",
	"bitsong1gdptewdmtc80qtlhm2w2ksw0xjy7c0qknq0f74",
	"bitsong1ffun6zxuuvq3dteh5h2agqedvaexntnft90tr5",
	"bitsong1nphhydjshzjevd03afzlce0xnlrnsm27hy9hgd",
	"bitsong1kr9phxmf5efd7z0mf3nmr6k8qt7u8pwvlcxvg5",
	"bitsong1klg4erpv2rn3txm72qs5gwwxwwy36fzz4mggd7",
	"bitsong16y4ex54wdu4e8mvjjx949s2ywnk5a2lacwwl04",
	"bitsong1u7ef57csrlnga9v2q2v5042vlckjhgd7cz2zws",
	"bitsong1a5mn8x9kp9tft7ucfgaj2mm72fpk0mf9ktjvna",
	"bitsong17086qrqupmv3gr9pxmfv2fhdys6rea2c34mp28",
	"bitsong1l77wstfdf08f2x8al0m52hru4z2tm0ug0f7kua",
}

// VO18 validators with slashed events
var SLASHED_VALIDATORS = []string{
	"bitsongvaloper100akrazwzrxhklgnh2ueaqfcv7kcanh9ew3jxf",
	"bitsongvaloper10gwt8pf92qnf4ym42xs593gxfmwze57vtw0yw8",
	"bitsongvaloper10uv3t6yru5dryz2yy9em2pzmqezyhsp0gkkxd2",
	"bitsongvaloper10y23e2v9hm82uhw4m5zhm3mxrrqc865dmanyl5",
	"bitsongvaloper125ph4snec4yneqagk5u07e95sej4zhjcjw3yx9",
	"bitsongvaloper128fz9ch8230nmgndn9rwg8wzk3m2ne5sh4vgq0",
	"bitsongvaloper12a627cpjyez9ne06nttzqrgt067znnhqqlguv0",
	"bitsongvaloper12d0dae3qm4et9r5l2npwrmwknax07yd0rrlhxx",
	"bitsongvaloper12gt92p97xhwd8xsp0f54nadzc7r2utakvverr8",
	"bitsongvaloper12tnf2vmfxjat0c2qcrr32h42jxr5vuwj6hn2qs",
	"bitsongvaloper139dppl6gyerq8yaweksajut3urwyygsz7r4ej4",
	"bitsongvaloper13g02l4y2es8ppls7ar5zh58ldre8q7tsv6kqer",
	"bitsongvaloper13q3m6kndt0z0pla56mefde6uepacas7sdj8pru",
	"bitsongvaloper13rfuft69m3swst4ay7rugdy6pqg2un3tyw7px0",
	"bitsongvaloper14uza97j6d0tcgdffx24x9kg7mz07hxmwcggtj2",
	"bitsongvaloper14yxgr6tpsta2lnuxtrqd4ajqnqdgcffnvzer4n",
	"bitsongvaloper159lye6negcv54ad4chklgkch43xfg9t6gp3jlm",
	"bitsongvaloper15y849ymzrdx88j79dx9l9548wzx2xapgxzq4f0",
	"bitsongvaloper16cm4ew7zrzaa7rf250jhwq2lcz3a60xmw0h5v8",
	"bitsongvaloper16r7fdhkusskwjymqx7pehhnuka5x0v8whm4h56",
	"bitsongvaloper17343qmv2gpe79t5f87gqg3m6vzfz33z9ssh5x0",
	"bitsongvaloper17avy3w8jejz34kazksdq2we46nykpjxa7tuvln",
	"bitsongvaloper17dpklyxlrn9kypkd3khy9t98v8qddnghllnt7x",
	"bitsongvaloper184mwu6n0m7cl30d3ku2jjfhvh0nuf2stslvewx",
	"bitsongvaloper184qrd7ucdtker9evyqu4lgh7fx5hgv7z8mzz4y",
	"bitsongvaloper189y5r7c3a9a2kpthd5ah6l7za2jz7p8yw3rz9f",
	"bitsongvaloper18nqd69k3kgns9tyvq8e8supsvx9wgwwu60c9np",
	"bitsongvaloper18wf0w252jxk3kgl5vlst8ttat8xzfnvejuftk2",
	"bitsongvaloper19h3jk2e8mxfz78fajgp39kp0gkr088934rv3hr",
	"bitsongvaloper19mmq66klqpcqjztdaclaf5tvknn4mjkd9k9fup",
	"bitsongvaloper19qtzdsu57hf5jmcyy5t2uuh0y45q4ah7hrwk4n",
	"bitsongvaloper1aa4z7tyml6tcw5arckfx3xqszcedn4gu8su8gz",
	"bitsongvaloper1aakd7kru6n9l2ty565j3t893ygadc9xz0c65mk",
	"bitsongvaloper1ajzyxcy3ecnya2pmtr353esl05pfwaglktcjmx",
	"bitsongvaloper1av9wz7f2kn8wd5pdw9j99nyf533re9dljxnxf2",
	"bitsongvaloper1cqj3wm5pcnz8dcjlgpj465ec83hvsgpc50c3le",
	"bitsongvaloper1d43njpcazljg950ypm7c87vg5r0huh3j2kf7zm",
	"bitsongvaloper1d85qa2wd7ny7dp0hft3u3tplwjdq3ttc2walgf",
	"bitsongvaloper1dlrrtjg4gf6y3l03fkj7jsefu5dhldsmt39tzg",
	"bitsongvaloper1dn89hnfs8f85j3plzqc9my5ntfqrrus3lx0y2m",
	"bitsongvaloper1dqzmtlrh5krqlrz694cxnkqf4y3wyqv3tmhn07",
	"bitsongvaloper1dthr54nysxnltnaxk4nmx2udqvlst8e5qh2yzw",
	"bitsongvaloper1dva44xz4mz2hq06dz2ljssj64r54c056c8qdhe",
	"bitsongvaloper1e0hml2mljufyv8dcdy4z7l2f3e8psxuddrqudr",
	"bitsongvaloper1e2ag6yrrqj6rqeprc24fw8rg90g54wv2j85nza",
	"bitsongvaloper1e2kpapes4zgsw9yzf2vfa6sjw3pe8m9lnh9y4l",
	"bitsongvaloper1e5w9w2aqp6mz6tsyregj8gvcpyunkcwlwvywxs",
	"bitsongvaloper1e6ds0qw6spsnhp4eatctafqarjyf3r0gfvslc7",
	"bitsongvaloper1e6ueftq485nqqptlj7kjpva8yhjlvu09csxmyp",
	"bitsongvaloper1ec5p8g5x2ldahexzdvl4373h7fwf2g6mfjh0qh",
	"bitsongvaloper1ech3swu5jeuenxzgd0rq9xz6yz3yn6t0v2x0tw",
	"bitsongvaloper1eflcvhl567dsdf0qlz48jth89a8qv8tytxpllz",
	"bitsongvaloper1elkz76q3nxvldun2kg6zx53vxg82clt47x9d6k",
	"bitsongvaloper1eydrxy4jn2zv38xd0xswhhqzgf5cygead8ly4g",
	"bitsongvaloper1f29dnzpsway54pwaqzp304st9wp5duy2zdxnnt",
	"bitsongvaloper1f4z9xvfswjyss32d26z8v3ak5f97t74zj5c6ht",
	"bitsongvaloper1f7qapscfpqzhwvhulc7pepd66wqd3963cq2p43",
	"bitsongvaloper1fk4up90cx352jwr6clug8pkg5jt28ha9whf6pu",
	"bitsongvaloper1fkj2cn209yeexxyets98evrcmmds23hck0lyzq",
	"bitsongvaloper1gf39p555c565804assht6tqvgrjgpyn30wssq8",
	"bitsongvaloper1gfrxhsdleg4v3vqqd0h8pv6ln96jrsyyx6swxy",
	"bitsongvaloper1h6a9l0ssh4n9lqgcfanqf7702k3980gl7jyx6c",
	"bitsongvaloper1h8galdzmm8cn9e7hrs3s0ae2j2hzxhnn98cdwg",
	"bitsongvaloper1hgh0pgpjmvcfsyl3qg7axux0fx4q469mr3z2pk",
	"bitsongvaloper1hzdp3kcgxz62tc9na39l3tj7h9gyzsf4p0nr5f",
	"bitsongvaloper1j0mpryq0r0gl80xty6x7u7a83gelfd70zxrx49",
	"bitsongvaloper1j4mxy83tg08yu24u6c0uu23fvr0v0h9qwaarnh",
	"bitsongvaloper1j98m4tzhzktqgwmmd3q8k9trgch5ssxnpm7r3k",
	"bitsongvaloper1jsaud5d8weze74a5e5w9ercxamtglg9wapc43m",
	"bitsongvaloper1jz8kddkl59jlnx7tkgu4pkzk8r4ztrfmg0lujf",
	"bitsongvaloper1kmngdftrv8rq79eswfw0k3z26ah77h5luacg0e",
	"bitsongvaloper1l4g6s4hstt6qfmakrthcjjqt47l2hgv3202ckl",
	"bitsongvaloper1lmxl3lyhpy8fhwa00mz09s333ejzgem0elef2s",
	"bitsongvaloper1lsnytfe0uxtzg860uv72rjeulhhujxu6cjsc8c",
	"bitsongvaloper1lw4aqn2ce65tp80swj5gfz4hhw9hug8hd7ukrs",
	"bitsongvaloper1m8ps45ltlt0vejjm2hqtu26jkd8rfkz9vwu5tw",
	"bitsongvaloper1mf4f7xcqx3x5uuq4vj86p6ck3fqmk299seukp6",
	"bitsongvaloper1mx3gct8chrssamkdfw8fkrdl93knllryalmxpm",
	"bitsongvaloper1mz5q43032m6qwzsq2e3vkjgs7zqx4ksl7swf9v",
	"bitsongvaloper1n2pxjj5805wvhx409a47dzj7g5e46zny7htddh",
	"bitsongvaloper1n4a5nydjphwkhf88n0sl38qceea4wfzwmnmz54",
	"bitsongvaloper1n6tyutks07pwxzdlu5vvn38va34ty8jdsk3ce3",
	"bitsongvaloper1n7jt63x5yw7w28a39xa8vs55dk5s3kqw33fmul",
	"bitsongvaloper1nj08dyw3q0fayh3y203x7a3zr3kzh79330ffef",
	"bitsongvaloper1nw4wmjq7le0h993tn27kmnqk2y8mdvhutzklgk",
	"bitsongvaloper1pd7vtq8grrhv24kz7z7m5rgue9ukm60cr9v8sz",
	"bitsongvaloper1pn6mhrwq8vdhns366jwxkggmhmxypuhrrc4huu",
	"bitsongvaloper1qcgyjxjcr4tv7m36sv38s4njjt63ll42xar457",
	"bitsongvaloper1qwpqjehpfsvaqhprg6hr0nawfuam24q65qdpw8",
	"bitsongvaloper1qxw4fjged2xve8ez7nu779tm8ejw92rv0vcuqr",
	"bitsongvaloper1rray2m6aggh9l44zzhf46fvnwn04w8j0rwy4e3",
	"bitsongvaloper1rtxprc6emy5jnqm88ssf0agernku83s2rg5tm7",
	"bitsongvaloper1slkjvzezgq8qxkg4p8yhaslvswr6gu97rjc6e5",
	"bitsongvaloper1slnkc2a8lhxgz5cc7lg9zlgzfedfpdve0rh2p6",
	"bitsongvaloper1sr97dlwwc3420dyxg50cldm4yqxtl7x2y4tzy2",
	"bitsongvaloper1stxt50ygdlfwu7erkyps3j4wfq6vx935ry53ne",
	"bitsongvaloper1t26ag4sn8075dk27d8zp6dh30prnqzymcnee5a",
	"bitsongvaloper1tfvasglxne4ftuxuedxhtfsgd2x6dwggrmc5y9",
	"bitsongvaloper1um889kzz9gyxlwtrgs6mc9sdwwx8z2kekudytd",
	"bitsongvaloper1vkgs96yclhvlk9jemetct6rj8katjnvp7ts7jr",
	"bitsongvaloper1vtz6a2lrc0ckmrym52n3ryffdszy0j7le6epm2",
	"bitsongvaloper1vxwt69pwh0nxjpuw8w7q8u0tema8rxa9hhgx4s",
	"bitsongvaloper1w40wm0hzakvekxfq93mncjcd7ss30futm5jqjg",
	"bitsongvaloper1wn7utf3p4pvudeudqx8l2dj6qsjve0uevtfc2p",
	"bitsongvaloper1wnhryryc0k3m92ch7qvtpc848f3ket83nxvazj",
	"bitsongvaloper1wq3hjdygz9n84z826p56zft0e45kmlfrnsy4p4",
	"bitsongvaloper1wtf99e8l5k2yxxc0rj6xwxj7mfam9hfnjqznja",
	"bitsongvaloper1wtycu6ypkap6r5yxs9whzkdpq9cldl3dksqtdx",
	"bitsongvaloper1x2slrjfmgxq7qx3xwmsjue73t5ynmwy7jgpdtr",
	"bitsongvaloper1xnc32z84cc9vwftvv4w0v02a2slug3tjt6qyct",
	"bitsongvaloper1xp2vhupfmhrpg5a2eajsp3s5va6zuv27yvczc5",
	"bitsongvaloper1xtxvvahw35cepycmsrvjtzty4fy6y4yh3qyrnr",
	"bitsongvaloper1xwazl8ftks4gn00y5x3c47auquc62ssugxgm5z",
	"bitsongvaloper1xzaknf4rzk5edx6gewgqxk29m0tt73h36uyg68",
	"bitsongvaloper1y0lhvwxg9av3zwhkjnlhtm89kx0w5enrncrrxk",
	"bitsongvaloper1y2gsual9m6a3twsc662f0fnfcm7awsrn8rwzrm",
	"bitsongvaloper1y3eqde9yckr0kfevnh9uwfd8gs0mek99rj29t7",
	"bitsongvaloper1y3sz27dfqh3fa5q305sx9s5lje07efr0cz2q2u",
	"bitsongvaloper1yuz9uukw8484rmsvhh3ztazl4xghyc8mv2j97s",
	"bitsongvaloper1zcez38u4929hwlygzs8dc4l9p45sfq00junld4",
	"bitsongvaloper1zuc0kljzv5mutgk0ddufp4vgvpy04dhz07dlxp",
	"bitsongvaloper1zy9xd2lz6df6gf55nq0ldase9sxtxlksdvfum2",
}

func (k Keeper) withdrawDelegationRewards(ctx sdk.Context, val stakingtypes.ValidatorI, del stakingtypes.DelegationI) (sdk.Coins, error) {
	// check existence of delegator starting info
	if !k.HasDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr()) {
		return nil, types.ErrEmptyDelegationDistInfo
	}

	// end current period and calculate rewards
	endingPeriod := k.IncrementValidatorPeriod(ctx, val)
	rewardsRaw := k.CalculateDelegationRewards(ctx, val, del, endingPeriod)
	outstanding := k.GetValidatorOutstandingRewardsCoins(ctx, del.GetValidatorAddr())

	// defensive edge case may happen on the very final digits
	// of the decCoins due to operation order of the distribution mechanism.
	rewards := rewardsRaw.Intersect(outstanding)
	if !rewards.IsEqual(rewardsRaw) {
		logger := k.Logger(ctx)
		logger.Info(
			"rounding error withdrawing rewards from validator",
			"delegator", del.GetDelegatorAddr().String(),
			"validator", val.GetOperator().String(),
			"got", rewards.String(),
			"expected", rewardsRaw.String(),
		)
	}

	// truncate reward dec coins, return remainder to community pool
	finalRewards, remainder := rewards.TruncateDecimal()

	// add coins to user account
	if !finalRewards.IsZero() {
		withdrawAddr := k.GetDelegatorWithdrawAddr(ctx, del.GetDelegatorAddr())
		err := k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, withdrawAddr, finalRewards)
		if err != nil {
			return nil, err
		}
	}

	// update the outstanding rewards and the community pool only if the
	// transaction was successful
	k.SetValidatorOutstandingRewards(ctx, del.GetValidatorAddr(), types.ValidatorOutstandingRewards{Rewards: outstanding.Sub(rewards)})
	feePool := k.GetFeePool(ctx)
	feePool.CommunityPool = feePool.CommunityPool.Add(remainder...)
	k.SetFeePool(ctx, feePool)

	// decrement reference count of starting period
	startingInfo := k.GetDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())
	startingPeriod := startingInfo.PreviousPeriod
	k.decrementReferenceCount(ctx, del.GetValidatorAddr(), startingPeriod)

	// remove delegator starting info
	k.DeleteDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())

	if finalRewards.IsZero() {
		baseDenom, _ := sdk.GetBaseDenom()
		if baseDenom == "" {
			baseDenom = sdk.DefaultBondDenom
		}

		// Note, we do not call the NewCoins constructor as we do not want the zero
		// coin removed.
		finalRewards = sdk.Coins{sdk.NewCoin(baseDenom, math.ZeroInt())}
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeWithdrawRewards,
			sdk.NewAttribute(sdk.AttributeKeyAmount, finalRewards.String()),
			sdk.NewAttribute(types.AttributeKeyValidator, val.GetOperator().String()),
			sdk.NewAttribute(types.AttributeKeyDelegator, del.GetDelegatorAddr().String()),
		),
	)

	return finalRewards, nil
}
