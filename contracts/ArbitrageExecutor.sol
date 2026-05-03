// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title ArbitrageExecutor
 * @notice Educational DEX arbitrage contract.
 *         Executes atomic 2-hop swaps across two Uniswap V2-compatible pools.
 *
 * @dev Key concepts demonstrated:
 *
 *  1. ATOMICITY
 *     All swaps happen in one transaction. If any step fails,
 *     the entire transaction reverts — you lose no funds except gas.
 *
 *  2. SLIPPAGE PROTECTION
 *     `amountOutMin` prevents execution if the price moved too much
 *     between when you built the tx and when it was included in a block.
 *
 *  3. DEADLINE
 *     Prevents a "stale" transaction from executing hours later when
 *     the opportunity is long gone.
 *
 *  4. FLASH LOAN CALLBACK (executeOperation)
 *     Implements the AAVE flash loan receiver interface.
 *     The loan is repaid within the same transaction.
 *
 *  SECURITY NOTES (production checklist):
 *  - Add onlyOwner modifier to sensitive functions
 *  - Add reentrancy guard (OpenZeppelin's ReentrancyGuard)
 *  - Validate pool addresses against a whitelist
 *  - Add emergency withdraw
 *  - Audit before deploying with real funds
 */

interface IERC20 {
    function balanceOf(address account) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
}

interface IUniswapV2Pair {
    function getReserves() external view returns (
        uint112 reserve0, uint112 reserve1, uint32 blockTimestampLast
    );
    function swap(
        uint256 amount0Out,
        uint256 amount1Out,
        address to,
        bytes calldata data
    ) external;
    function token0() external view returns (address);
    function token1() external view returns (address);
}

// AAVE flash loan interface (v2)
interface ILendingPool {
    function flashLoan(
        address receiverAddress,
        address[] calldata assets,
        uint256[] calldata amounts,
        uint256[] calldata modes,  // 0 = no debt (must repay in same tx)
        address onBehalfOf,
        bytes calldata params,
        uint16 referralCode
    ) external;
}

contract ArbitrageExecutor {
    address public owner;
    address public immutable aaveLendingPool;

    // Events help off-chain monitoring tools track executions
    event ArbitrageExecuted(
        address indexed tokenIn,
        address indexed tokenOut,
        uint256 amountIn,
        uint256 profit,
        address buyPool,
        address sellPool
    );
    event FlashLoanArbitrageExecuted(
        address indexed loanToken,
        uint256 loanAmount,
        uint256 profit
    );

    modifier onlyOwner() {
        require(msg.sender == owner, "ArbitrageExecutor: not owner");
        _;
    }

    constructor(address _aaveLendingPool) {
        owner = msg.sender;
        aaveLendingPool = _aaveLendingPool;
    }

    /**
     * @notice Execute a simple 2-hop arbitrage using your own capital.
     *
     * Flow:
     *   1. Transfer amountIn of tokenIn from caller to this contract
     *   2. Approve buyPool to spend tokenIn
     *   3. Swap tokenIn → tokenOut on buyPool
     *   4. Approve sellPool to spend tokenOut
     *   5. Swap tokenOut → tokenIn on sellPool
     *   6. Verify profit ≥ 0 (or revert)
     *   7. Transfer all tokenIn back to caller
     *
     * @param tokenIn      The token we start with (e.g. USDC)
     * @param tokenOut     The intermediate token (e.g. WETH)
     * @param amountIn     Amount of tokenIn to deploy
     * @param amountOutMin Minimum tokenIn to receive back (slippage guard)
     * @param poolBuy      Uniswap-V2-compatible pool to buy tokenOut on
     * @param poolSell     Uniswap-V2-compatible pool to sell tokenOut on
     * @param deadline     Revert if block.timestamp > deadline
     * @return profit      Net profit in tokenIn units
     */
    function executeArbitrage(
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        uint256 amountOutMin,
        address poolBuy,
        address poolSell,
        uint256 deadline
    ) external onlyOwner returns (uint256 profit) {
        require(block.timestamp <= deadline, "ArbitrageExecutor: expired");
        require(amountIn > 0, "ArbitrageExecutor: amountIn is zero");

        // Step 1: pull funds from caller
        IERC20(tokenIn).transferFrom(msg.sender, address(this), amountIn);

        // Step 2 & 3: buy tokenOut on poolBuy
        uint256 tokenOutAmount = _swapExactIn(tokenIn, tokenOut, amountIn, poolBuy);

        // Step 4 & 5: sell tokenOut on poolSell, receive tokenIn
        uint256 tokenInBack = _swapExactIn(tokenOut, tokenIn, tokenOutAmount, poolSell);

        // Step 6: slippage / profitability check
        require(tokenInBack >= amountOutMin, "ArbitrageExecutor: insufficient output");

        profit = tokenInBack - amountIn;

        // Step 7: return all funds to caller
        IERC20(tokenIn).transfer(msg.sender, tokenInBack);

        emit ArbitrageExecuted(tokenIn, tokenOut, amountIn, profit, poolBuy, poolSell);
    }

    /**
     * @notice Initiate a flash loan arbitrage (borrow without collateral).
     *
     * Flow:
     *   1. Call AAVE flashLoan() with the desired borrow amount
     *   2. AAVE calls executeOperation() (below) on this contract
     *   3. Inside executeOperation(): run the arb, repay loan + fee
     *   4. This contract keeps the net profit
     *
     * @param loanToken    Token to borrow from AAVE
     * @param loanAmount   Amount to borrow
     * @param tokenOut     Intermediate token for the arb
     * @param amountOutMin Minimum tokens to receive back
     * @param poolBuy      Pool to buy on
     * @param poolSell     Pool to sell on
     */
    function executeFlashLoanArb(
        address loanToken,
        uint256 loanAmount,
        address tokenOut,
        uint256 amountOutMin,
        address poolBuy,
        address poolSell
    ) external onlyOwner {
        address[] memory assets  = new address[](1);
        uint256[] memory amounts = new uint256[](1);
        uint256[] memory modes   = new uint256[](1);

        assets[0]  = loanToken;
        amounts[0] = loanAmount;
        modes[0]   = 0; // 0 = flash loan (must repay in same tx)

        // Pack arb parameters into the callback data
        bytes memory params = abi.encode(tokenOut, amountOutMin, poolBuy, poolSell);

        ILendingPool(aaveLendingPool).flashLoan(
            address(this), // receiver of the loan
            assets,
            amounts,
            modes,
            address(this),
            params,
            0 // referral code
        );
    }

    /**
     * @notice AAVE flash loan callback.
     *         Called by AAVE after transferring loan funds to this contract.
     *         Must repay amounts[i] + premiums[i] for each asset by end of call.
     *
     * This is the pattern every flash loan contract implements:
     *   1. Receive funds
     *   2. Execute strategy (arb, liquidation, etc.)
     *   3. Approve repayment amount
     *   4. Return true (signals AAVE to pull repayment)
     */
    function executeOperation(
        address[] calldata assets,
        uint256[] calldata amounts,
        uint256[] calldata premiums,
        address, // initiator (ignored – already validated via onlyOwner on the outer call)
        bytes calldata params
    ) external returns (bool) {
        require(msg.sender == aaveLendingPool, "ArbitrageExecutor: invalid caller");

        // Decode arb parameters
        (address tokenOut, uint256 amountOutMin, address poolBuy, address poolSell) =
            abi.decode(params, (address, uint256, address, address));

        address loanToken  = assets[0];
        uint256 loanAmount = amounts[0];
        uint256 fee        = premiums[0]; // AAVE charges 0.09% (9 bps)

        // Run the arbitrage with borrowed funds
        uint256 tokenOutAmount = _swapExactIn(loanToken, tokenOut, loanAmount, poolBuy);
        uint256 tokenInBack    = _swapExactIn(tokenOut, loanToken, tokenOutAmount, poolSell);

        uint256 repayAmount = loanAmount + fee;
        require(tokenInBack >= repayAmount + amountOutMin,
            "ArbitrageExecutor: flash loan arb not profitable");

        // Approve AAVE to pull repayment
        IERC20(loanToken).approve(aaveLendingPool, repayAmount);

        uint256 profit = tokenInBack - repayAmount;
        emit FlashLoanArbitrageExecuted(loanToken, loanAmount, profit);

        return true; // success signal to AAVE
    }

    /**
     * @dev Internal: execute a single swap on a Uniswap V2-compatible pool.
     *      Uses the low-level pair.swap() interface for gas efficiency.
     *      The standard Router adds ~30k gas; direct pair calls save that.
     */
    function _swapExactIn(
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        address pair
    ) internal returns (uint256 amountOut) {
        // Calculate output using the constant product formula (same as amm.go)
        (uint112 reserve0, uint112 reserve1,) = IUniswapV2Pair(pair).getReserves();
        address token0 = IUniswapV2Pair(pair).token0();

        (uint256 reserveIn, uint256 reserveOut) = (tokenIn == token0)
            ? (reserve0, reserve1)
            : (reserve1, reserve0);

        // Uniswap V2 formula (fee = 0.3%)
        uint256 amountInWithFee = amountIn * 997;
        amountOut = (amountInWithFee * reserveOut) / (reserveIn * 1000 + amountInWithFee);

        // Transfer tokenIn to the pair (pairs pull from msg.sender)
        IERC20(tokenIn).transfer(pair, amountIn);

        // Call swap with correct amount ordering
        (uint256 out0, uint256 out1) = (tokenIn == token0)
            ? (uint256(0), amountOut)
            : (amountOut, uint256(0));

        IUniswapV2Pair(pair).swap(out0, out1, address(this), "");
    }

    /**
     * @notice Withdraw accumulated profits.
     * @param token ERC-20 token to withdraw (send entire balance to owner)
     */
    function withdraw(address token) external onlyOwner {
        uint256 bal = IERC20(token).balanceOf(address(this));
        require(bal > 0, "ArbitrageExecutor: nothing to withdraw");
        IERC20(token).transfer(owner, bal);
    }

    /// @notice Transfer ownership (e.g. to a multisig for production)
    function transferOwnership(address newOwner) external onlyOwner {
        require(newOwner != address(0), "ArbitrageExecutor: zero address");
        owner = newOwner;
    }
}
