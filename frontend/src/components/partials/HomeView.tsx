export const HomeView = () => {
  return (
    <div className=' w-full -z-30 h-screen'>
      <svg
        id='visual'
        xmlns='http://www.w3.org/2000/svg'
        version='1.1'
        className=" w-full h-full"
      >
        <rect x='0' y='0' width='900' height='600' fill='#9f0000'></rect>
        <defs>
          <linearGradient id='grad1_0' x1='33.3%' y1='100%' x2='100%' y2='0%'>
            <stop offset='20%' stop-color='#9f0000' stop-opacity='1'></stop>
            <stop offset='80%' stop-color='#9f0000' stop-opacity='1'></stop>
          </linearGradient>
        </defs>
        <defs>
          <linearGradient id='grad2_0' x1='0%' y1='100%' x2='66.7%' y2='0%'>
            <stop offset='20%' stop-color='#9f0000' stop-opacity='1'></stop>
            <stop offset='80%' stop-color='#9f0000' stop-opacity='1'></stop>
          </linearGradient>
        </defs>
        <g transform='translate(900, 600)'>
          <path
            d='M-324.5 0C-294.5 -34.1 -264.6 -68.2 -246.7 -102.2C-228.8 -136.1 -222.9 -169.9 -205.8 -205.8C-188.7 -241.7 -160.3 -279.7 -124.2 -299.8C-88 -319.9 -44 -322.2 0 -324.5L0 0Z'
            fill='#F7770F'
          ></path>
        </g>
        <g transform='translate(0, 0)'>
          <path
            d='M324.5 0C303.7 38.3 282.8 76.6 262.4 108.7C241.9 140.7 221.8 166.6 200.8 200.8C179.8 235.1 157.8 277.8 124.2 299.8C90.6 321.8 45.3 323.2 0 324.5L0 0Z'
            fill='#F7770F'
          ></path>
        </g>
      </svg>
    </div>
  );
};
