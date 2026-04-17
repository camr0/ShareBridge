import { build, context } from 'esbuild';

const opts = {
  entryPoints: ['src/app.js'],
  outfile: 'app.bundle.js',
  bundle: true,
  format: 'esm',
  target: ['es2022'],
  platform: 'browser',
  sourcemap: true,
  minify: process.env.NODE_ENV === 'production',
  loader: { '.js': 'js' },
};

if (process.argv.includes('--watch')) {
  const ctx = await context(opts);
  await ctx.watch();
  console.log('watching...');
} else {
  await build(opts);
}