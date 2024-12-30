/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ['./src/**/*.{html,ts}', './templates/**/*.{html,tmpl,gohtml}'],
  theme: {
    extend: {},
  },
  plugins: [
    require('@tailwindcss/typography'),
    require('@tailwindcss/forms')({ strategy: 'class' }),
    require('@tailwindcss/aspect-ratio'),
    require('daisyui'),
    require('tailwind-scrollbar-hide'),
  ],
};
