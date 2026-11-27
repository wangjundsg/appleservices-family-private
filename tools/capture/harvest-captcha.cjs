"use strict";
// Both historical entry names now use the same webpage-only collector.
require('./harvest-captcha-ui.cjs').main().catch(error=>{
  console.error(JSON.stringify({state:'failed',error:error.message}));process.exitCode=1;
});
