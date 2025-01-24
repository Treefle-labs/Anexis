import { Suspense } from 'react';
import App from '../components/App';
import { HomeView } from '../components/partials/HomeView';

function Home() {
  return (
    <Suspense fallback={<>Loading</>}>
      <App>
        <HomeView />
      </App>
    </Suspense>
  );
}

export default Home;
