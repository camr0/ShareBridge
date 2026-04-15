const path = require('path')
const webpack = require('webpack')
const webpackConfig = require('@nextcloud/webpack-vue-config')

// Remove the default TS rule so we can replace it with our configured version
const rulesWithoutTS = webpackConfig.module.rules.filter(rule => !rule.test?.test?.('.ts'))

module.exports = {
    ...webpackConfig,
    entry: {
        'sharebridge-main': path.join(__dirname, 'src', 'main.ts'),
    },
    output: {
        ...webpackConfig.output,
        filename: '[name].js',
    },
    plugins: [
        ...(webpackConfig.plugins || []),
        // Vue 3 esm-bundler builds require these compile-time flags to be defined
        new webpack.DefinePlugin({
            __VUE_OPTIONS_API__: JSON.stringify(true),
            __VUE_PROD_DEVTOOLS__: JSON.stringify(false),
            __VUE_PROD_HYDRATION_MISMATCH_DETAILS__: JSON.stringify(false),
        }),
    ],
    module: {
        rules: [
            ...rulesWithoutTS,
            {
                test: /\.tsx?$/,
                use: [
                    'babel-loader',
                    {
                        loader: 'ts-loader',
                        options: {
                            appendTsSuffixTo: [/\.vue$/],
                            transpileOnly: true,
                        },
                    },
                ],
                exclude: /node_modules/,
            },
        ],
    },
}